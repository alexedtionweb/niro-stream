package middleware_test

import (
	"context"
	"testing"
	"time"

	"ryn.dev/ryn"
	"ryn.dev/ryn/middleware"
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
