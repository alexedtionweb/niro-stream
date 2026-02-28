package middleware_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"ryn.dev/ryn"
	"ryn.dev/ryn/middleware"
)

func TestDefaultTimeoutConfig(t *testing.T) {
	t.Parallel()
	cfg := middleware.DefaultTimeoutConfig()
	assertEqual(t, cfg.GenerationTimeout, 5*time.Minute)
	assertEqual(t, cfg.FrameTimeout, 30*time.Second)
	assertEqual(t, cfg.ToolTimeout, 1*time.Minute)
}

func TestTimeoutProviderSuccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("hello")}), nil
	})

	provider := middleware.NewTimeoutProvider(mock, 5*time.Second)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertNoError(t, err)
	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "hello")
}

func TestTimeoutProviderDefaultOnZero(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	// Zero timeout → uses default (5 minutes), should still work for fast providers
	provider := middleware.NewTimeoutProvider(mock, 0)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, stream)
	assertEqual(t, text, "ok")
}

func TestTimeoutProviderEnforces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Provider that blocks until its context is cancelled.
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		out, em := ryn.NewStream(4)
		go func() {
			defer em.Close()
			<-ctx.Done() // unblocks when timeout fires
		}()
		return out, nil
	})

	provider := middleware.NewTimeoutProvider(mock, 50*time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
		if err != nil {
			return
		}
		for stream.Next(ctx) {
		}
	}()

	select {
	case <-done:
		// Good — completed within the timeout window
	case <-time.After(3 * time.Second):
		t.Fatal("timeout provider did not enforce the timeout")
	}
}

func TestComposedBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("composed")}), nil
	})

	// Composed with timeout only, no retry
	provider := middleware.Composed(mock, 30*time.Second, nil)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, stream)
	assertEqual(t, text, "composed")
}

func TestComposedWithRetryConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	attempts := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		attempts++
		if attempts < 2 {
			return nil, ryn.NewError(ryn.ErrCodeRateLimited, "rate limited")
		}
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	retryConfig := middleware.DefaultRetryConfig()
	retryConfig.MaxAttempts = 3
	retryConfig.Backoff = middleware.ConstantBackoff{Duration: time.Millisecond}

	provider := middleware.Composed(mock, 30*time.Second, &retryConfig)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, stream)
	assertEqual(t, text, "ok")
	assertTrue(t, attempts >= 2)
}

func TestWithGenerationTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tctx, cancel := middleware.WithGenerationTimeout(ctx, 5*time.Second)
	defer cancel()

	deadline, ok := tctx.Deadline()
	assertTrue(t, ok)
	assertTrue(t, time.Until(deadline) > 0)
	assertTrue(t, time.Until(deadline) <= 5*time.Second)
}

func TestTimeoutProviderProviderError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, fmt.Errorf("provider down")
	})

	provider := middleware.NewTimeoutProvider(mock, 5*time.Second)
	_, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertErrorContains(t, err, "provider down")
}

func TestTimeoutProviderEmitCancelledConsumer(t *testing.T) {
	t.Parallel()

	// The emitter.Emit(tctx, frame) early-return branch fires when the timeout
	// context is cancelled while Emit is blocking (output buffer full).
	// Strategy: fast producer fills the output buffer (size 32), then the
	// timeout fires while Emit is blocking on a full channel.
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		// emit 64 frames immediately via a buffered stream
		frames := make([]ryn.Frame, 64)
		for i := range frames {
			frames[i] = ryn.TextFrame(fmt.Sprintf("f%d", i))
		}
		return ryn.StreamFromSlice(frames), nil
	})

	// Very short timeout fires while the 32-slot output buffer is saturated
	// and no consumer is draining — Emit(tctx, ...) blocks then tctx fires.
	provider := middleware.NewTimeoutProvider(mock, 1*time.Millisecond)
	// Don't drain the stream — let the 32-buffer fill so Emit blocks.
	stream, err := provider.Generate(context.Background(), &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}})
	assertNoError(t, err)

	// Wait just long enough for the timeout to fire, then drain.
	time.Sleep(5 * time.Millisecond)
	for stream.Next(context.Background()) {
	}
	_ = stream.Err()
}

func TestTimeoutProviderForwardsResponseAndUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		out, em := ryn.NewStream(8)
		go func() {
			defer em.Close()
			_ = em.Emit(ctx, ryn.TextFrame("hello"))
			em.SetResponse(&ryn.ResponseMeta{Model: "gpt-test", FinishReason: "stop"})
			u := ryn.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}
			_ = em.Emit(ctx, ryn.UsageFrame(&u))
		}()
		return out, nil
	})

	provider := middleware.NewTimeoutProvider(mock, 5*time.Second)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}})
	assertNoError(t, err)

	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "hello")

	resp := stream.Response()
	assertNotNil(t, resp)
	assertEqual(t, resp.FinishReason, "stop")

	u := stream.Usage()
	assertEqual(t, u.InputTokens, 7)
	assertEqual(t, u.TotalTokens, 10)
}
