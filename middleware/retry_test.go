package middleware_test

import (
	"context"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/middleware"
)

func TestRetryProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	attempts := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		attempts++
		if attempts < 3 {
			return nil, ryn.NewError(ryn.ErrCodeRateLimited, "too fast")
		}
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	config := middleware.RetryConfig{
		MaxAttempts: 5,
		Backoff:     middleware.ConstantBackoff{Duration: 5 * time.Millisecond},
		ShouldRetry: ryn.IsRetryable,
	}

	provider := middleware.NewRetryProvider(mock, config)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})

	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, stream)
	assertEqual(t, text, "ok")
	assertEqual(t, attempts, 3)
}

func TestWrapWithSmartRetrySkipsWhenProviderRetries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	attempts := 0
	p := &retryHintProviderMock{
		handlesRetries: true,
		fn: func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
			attempts++
			return nil, ryn.NewError(ryn.ErrCodeRateLimited, "rate limited")
		},
	}

	cfg := middleware.DefaultRetryConfig()
	cfg.MaxAttempts = 4
	cfg.Backoff = middleware.ConstantBackoff{Duration: time.Millisecond}

	wrapped := middleware.WrapWithSmartRetry(p, cfg)
	_, _ = wrapped.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}})

	// Should not add another retry layer when provider says SDK retries internally.
	assertEqual(t, attempts, 1)
}

func TestExponentialBackoff(t *testing.T) {
	t.Parallel()

	backoff := middleware.ExponentialBackoff{
		InitialDelay: 10 * time.Millisecond,
		Multiplier:   2.0,
		MaxDelay:     100 * time.Millisecond,
		Jitter:       false,
	}

	d0 := backoff.Delay(0)
	d1 := backoff.Delay(1)
	d2 := backoff.Delay(2)

	assertTrue(t, d0 >= 10*time.Millisecond)
	assertTrue(t, d1 > d0)
	assertTrue(t, d2 > d1)
	assertTrue(t, d2 <= 100*time.Millisecond)
}

func TestExponentialBackoffWithJitter(t *testing.T) {
	t.Parallel()

	backoff := middleware.ExponentialBackoff{
		InitialDelay: 100 * time.Millisecond,
		Multiplier:   2.0,
		MaxDelay:     time.Second,
		Jitter:       true,
	}

	d0 := backoff.Delay(0)
	d1 := backoff.Delay(1)

	// With jitter, delays should be positive but may vary; they should be bounded.
	assertTrue(t, d0 > 0)
	assertTrue(t, d1 > 0)
	assertTrue(t, d0 <= time.Second)
	assertTrue(t, d1 <= time.Second)
}

func TestExponentialBackoffDefaults(t *testing.T) {
	t.Parallel()

	// Zero multiplier and maxDelay should use safe defaults.
	backoff := middleware.ExponentialBackoff{
		InitialDelay: 10 * time.Millisecond,
		Multiplier:   0, // → defaults to 2.0
		MaxDelay:     0, // → defaults to 5 minutes
	}

	d := backoff.Delay(0)
	assertTrue(t, d >= 10*time.Millisecond)
}

func TestNewRetryProviderNilDefaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	// Zero/nil values → defaults fill in: MaxAttempts=3, ExponentialBackoff, IsRetryable.
	config := middleware.RetryConfig{
		MaxAttempts: 0,
		Backoff:     nil,
		ShouldRetry: nil,
	}

	provider := middleware.NewRetryProvider(mock, config)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, stream)
	assertEqual(t, text, "ok")
}

func TestRetryNonRetryableError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	calls := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		calls++
		return nil, ryn.NewError(ryn.ErrCodeInvalidRequest, "bad request")
	})

	config := middleware.RetryConfig{
		MaxAttempts: 5,
		Backoff:     middleware.ConstantBackoff{Duration: time.Millisecond},
		ShouldRetry: ryn.IsRetryable,
	}

	provider := middleware.NewRetryProvider(mock, config)
	_, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertErrorContains(t, err, "bad request")
	// Should not retry a non-retryable error.
	assertEqual(t, calls, 1)
}

func TestRetryOnRetryCallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	callbackCount := 0
	attempts := 0

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		attempts++
		if attempts < 3 {
			return nil, ryn.NewError(ryn.ErrCodeRateLimited, "rate limited")
		}
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	config := middleware.RetryConfig{
		MaxAttempts: 5,
		Backoff:     middleware.ConstantBackoff{Duration: time.Millisecond},
		ShouldRetry: ryn.IsRetryable,
		OnRetry: func(attempt int, err error) {
			callbackCount++
		},
	}

	provider := middleware.NewRetryProvider(mock, config)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertNoError(t, err)
	ryn.CollectText(ctx, stream)

	// Two failures before success → callback called twice.
	assertEqual(t, callbackCount, 2)
}

func TestRetryContextCancelledDuringBackoff(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		cancel() // cancel immediately after first attempt
		return nil, ryn.NewError(ryn.ErrCodeRateLimited, "rate limited")
	})

	config := middleware.RetryConfig{
		MaxAttempts: 5,
		Backoff:     middleware.ConstantBackoff{Duration: time.Hour}, // very long delay
		ShouldRetry: ryn.IsRetryable,
	}

	provider := middleware.NewRetryProvider(mock, config)
	_, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertErrorContains(t, err, "cancel")
}

func TestRetryExhaustsAllAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	calls := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		calls++
		return nil, ryn.NewError(ryn.ErrCodeRateLimited, "always fails")
	})

	config := middleware.RetryConfig{
		MaxAttempts: 3,
		Backoff:     middleware.ConstantBackoff{Duration: time.Millisecond},
		ShouldRetry: ryn.IsRetryable,
	}

	provider := middleware.NewRetryProvider(mock, config)
	_, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertErrorContains(t, err, "attempts")
	assertEqual(t, calls, 3)
}

func TestExponentialBackoffMaxDelayClamped(t *testing.T) {
	t.Parallel()

	// Use a small MaxDelay so that attempt=5 exceeds it: 1ms * 2^5 = 32ms > 10ms.
	backoff := middleware.ExponentialBackoff{
		InitialDelay: 1 * time.Millisecond,
		Multiplier:   2.0,
		MaxDelay:     10 * time.Millisecond,
		Jitter:       false,
	}

	d := backoff.Delay(5) // 1ms * 32 = 32ms → clamped to 10ms
	assertEqual(t, d, 10*time.Millisecond)
}

func TestRetryContextPreCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before any attempt

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	config := middleware.RetryConfig{
		MaxAttempts: 3,
		Backoff:     middleware.ConstantBackoff{Duration: time.Millisecond},
		ShouldRetry: ryn.IsRetryable,
	}

	provider := middleware.NewRetryProvider(mock, config)
	_, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertErrorContains(t, err, "cancel")
}
