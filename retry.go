package ryn

import (
	"context"
	"math"
	"math/rand"
	"time"
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
		jitterValue := time.Duration(rand.Int63n(int64(jitterAmount)))
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
		ShouldRetry: IsRetryable,
		OnRetry:     nil,
	}
}

// RetryProvider wraps a Provider with automatic retry logic.
type RetryProvider struct {
	provider Provider
	config   RetryConfig
}

// NewRetryProvider creates a Provider that retries on transient failures.
func NewRetryProvider(p Provider, config RetryConfig) *RetryProvider {
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
		config.ShouldRetry = IsRetryable
	}
	return &RetryProvider{provider: p, config: config}
}

// Generate implements Provider with automatic retries.
func (rp *RetryProvider) Generate(ctx context.Context, req *Request) (*Stream, error) {
	var lastErr error

	for attempt := 0; attempt < rp.config.MaxAttempts; attempt++ {
		// Check context before each attempt
		select {
		case <-ctx.Done():
			return nil, NewErrorf(ErrCodeContextCancelled, "context cancelled after %d attempts", attempt)
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

		// Wait with context awareness
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, NewErrorf(ErrCodeContextCancelled, "context cancelled during retry backoff after %d attempts", attempt+1)
		case <-timer.C:
			// Continue to next attempt
		}
	}

	return nil, NewErrorf(ErrCodeProviderError, "all %d attempts failed: %v", rp.config.MaxAttempts, lastErr)
}

// RetryStream wraps a Stream with error recovery (for mid-stream failures).
// This allows partial results to be returned even if the stream encounters
// a transient error mid-generation.
type RetryStream struct {
	src  *Stream
	conf RetryConfig
}

// NewRetryStream creates a Stream that can recover from transient mid-stream errors.
func NewRetryStream(s *Stream, conf RetryConfig) *RetryStream {
	if conf.MaxAttempts <= 0 {
		conf.MaxAttempts = 2 // Fewer retries for mid-stream (we've already sent frames)
	}
	if conf.ShouldRetry == nil {
		conf.ShouldRetry = IsRetryable
	}
	return &RetryStream{src: s, conf: conf}
}

// Next reads the next frame, recovering from transient errors if possible.
func (rs *RetryStream) Next(ctx context.Context) bool {
	// For now, just delegate to underlying stream
	// In a future version, this could implement mid-stream recovery
	return rs.src.Next(ctx)
}

// Frame returns the current frame.
func (rs *RetryStream) Frame() Frame {
	return rs.src.Frame()
}

// Err returns any error that occurred.
func (rs *RetryStream) Err() error {
	return rs.src.Err()
}

// Usage returns accumulated token usage.
func (rs *RetryStream) Usage() Usage {
	return rs.src.Usage()
}

// Response returns response metadata.
func (rs *RetryStream) Response() *ResponseMeta {
	return rs.src.Response()
}
