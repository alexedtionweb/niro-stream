package middleware_test

import (
	"context"
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
