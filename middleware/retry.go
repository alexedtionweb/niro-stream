package middleware

import (
	"context"
	"math"
	"math/rand/v2"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

// BackoffStrategy defines how to calculate delays between retries.
type BackoffStrategy interface {
	// Delay returns the duration to wait before the nth attempt (0-indexed).
	Delay(attempt int) time.Duration
}

// ConstantBackoff uses a fixed delay between retries.
type ConstantBackoff struct {
	Duration time.Duration
}

func (cb ConstantBackoff) Delay(attempt int) time.Duration {
	return cb.Duration
}

// ExponentialBackoff uses exponential backoff with optional jitter.
// Formula: min(initialDelay * (multiplier ^ attempt) + jitter, maxDelay)
type ExponentialBackoff struct {
	InitialDelay time.Duration
	Multiplier   float64
	MaxDelay     time.Duration
	Jitter       bool // Add random jitter to avoid thundering herd
}

func (eb ExponentialBackoff) Delay(attempt int) time.Duration {
	if eb.Multiplier <= 0 {
		eb.Multiplier = 2.0
	}
	if eb.MaxDelay <= 0 {
		eb.MaxDelay = 5 * time.Minute
	}

	delay := time.Duration(float64(eb.InitialDelay) * math.Pow(eb.Multiplier, float64(attempt)))
	if delay > eb.MaxDelay {
		delay = eb.MaxDelay
	}

	if eb.Jitter && delay > 0 {
		jitterAmount := delay / 2
		jitterValue := time.Duration(rand.Int64N(int64(jitterAmount)))
		delay = delay - jitterAmount/2 + jitterValue
	}

	return delay
}

// RetryConfig configures retry behavior.
type RetryConfig struct {
	// Maximum number of attempts (including initial attempt)
	MaxAttempts int
	// Backoff strategy
	Backoff BackoffStrategy
	// Should retry based on error
	ShouldRetry func(err error) bool
	// Optional: callback on retry
	OnRetry func(attempt int, err error)

	// DisableWhenProviderRetries avoids double-retry when the underlying provider
	// or SDK already performs retries internally.
	DisableWhenProviderRetries bool
}

// DefaultRetryConfig returns sensible defaults: 3 attempts, exponential backoff with jitter.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 3,
		Backoff: ExponentialBackoff{
			InitialDelay: 100 * time.Millisecond,
			Multiplier:   2.0,
			MaxDelay:     10 * time.Second,
			Jitter:       true,
		},
		ShouldRetry:                ryn.IsRetryable,
		OnRetry:                    nil,
		DisableWhenProviderRetries: true,
	}
}

// RetryHintProvider is an optional provider extension that reports whether the
// underlying SDK/client already performs retries.
type RetryHintProvider interface {
	ProviderHandlesRetries() bool
}

// WrapWithSmartRetry adds retries only when they are useful.
//
// If cfg.DisableWhenProviderRetries is true and p indicates built-in retries,
// p is returned unchanged.
func WrapWithSmartRetry(p ryn.Provider, cfg RetryConfig) ryn.Provider {
	if cfg.DisableWhenProviderRetries {
		if hp, ok := p.(RetryHintProvider); ok && hp.ProviderHandlesRetries() {
			return p
		}
	}
	return NewRetryProvider(p, cfg)
}

// RetryProvider wraps a Provider with automatic retry logic.
type RetryProvider struct {
	provider ryn.Provider
	config   RetryConfig
}

// NewRetryProvider creates a Provider that retries on transient failures.
func NewRetryProvider(p ryn.Provider, config RetryConfig) *RetryProvider {
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 3
	}
	if config.Backoff == nil {
		config.Backoff = ExponentialBackoff{
			InitialDelay: 100 * time.Millisecond,
			Multiplier:   2.0,
			MaxDelay:     10 * time.Second,
			Jitter:       true,
		}
	}
	if config.ShouldRetry == nil {
		config.ShouldRetry = ryn.IsRetryable
	}
	return &RetryProvider{provider: p, config: config}
}

// Generate implements Provider with automatic retries.
func (rp *RetryProvider) Generate(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
	var lastErr error

	for attempt := 0; attempt < rp.config.MaxAttempts; attempt++ {
		// Check context before each attempt
		select {
		case <-ctx.Done():
			return nil, ryn.NewErrorf(ryn.ErrCodeContextCancelled, "context cancelled after %d attempts", attempt)
		default:
		}

		stream, err := rp.provider.Generate(ctx, req)
		if err == nil {
			return stream, nil
		}

		lastErr = err

		// Check if we should retry
		if !rp.config.ShouldRetry(err) {
			return nil, err
		}

		// If this was the last attempt, return the error
		if attempt == rp.config.MaxAttempts-1 {
			break
		}

		// Calculate backoff delay
		delay := rp.config.Backoff.Delay(attempt)

		// Call retry callback if provided
		if rp.config.OnRetry != nil {
			rp.config.OnRetry(attempt+1, err)
		}

		// Log the retry at Warn level so operators see transient failures without
		// noise from routine success paths. LogWarn propagates ctx so
		// context-aware adapters (e.g. OpenTelemetry) can correlate the record
		// with the originating request.
		ryn.LogWarn(ctx, "ryn/retry: retrying",
			"attempt", attempt+1,
			"max", rp.config.MaxAttempts,
			"delay", delay.String(),
			"err", err,
		)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ryn.NewErrorf(ryn.ErrCodeContextCancelled, "context cancelled during retry backoff after %d attempts", attempt+1)
		case <-timer.C:
			// Continue to next attempt
		}
	}

	return nil, ryn.NewErrorf(ryn.ErrCodeProviderError, "all %d attempts failed: %v", rp.config.MaxAttempts, lastErr)
}
