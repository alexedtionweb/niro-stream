package middleware

import (
	"context"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

// NewTimeoutProvider returns a Provider that enforces generation timeouts.
// It delegates to [niro.NewTimeoutProvider]; use [niro.TimeoutConfig] and
// [niro.DefaultTimeoutConfig] / [niro.TelephonyTimeoutConfig] for presets.
func NewTimeoutProvider(p niro.Provider, timeout time.Duration) niro.Provider {
	if timeout <= 0 {
		timeout = niro.DefaultTimeoutConfig().GenerationTimeout
	}
	return niro.NewTimeoutProvider(p, timeout)
}

// Composed combines multiple provider wrappers (timeout, tracing, retry).
// Useful for building a production-ready provider from building blocks.
// Apply order: retry (outermost) → timeout → tracing → base.
func Composed(p niro.Provider, timeout time.Duration, retryConfig *RetryConfig) niro.Provider {
	var composed niro.Provider = p

	// Innermost: tracing
	composed = NewTracingProvider(composed)

	// Middle: timeout
	if timeout > 0 {
		composed = NewTimeoutProvider(composed, timeout)
	}

	// Outermost: retry
	if retryConfig != nil {
		composed = WrapWithSmartRetry(composed, *retryConfig)
	}

	return composed
}

// WithGenerationTimeout returns a context with a generation timeout applied.
// Deprecated: use [niro.WithGenerationTimeout] instead.
func WithGenerationTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, timeout)
}
