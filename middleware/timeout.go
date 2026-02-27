package middleware

import (
	"context"
	"time"

	"ryn.dev/ryn"
)

// TimeoutConfig configures timeout behavior.
type TimeoutConfig struct {
	// GenerationTimeout is the max time for a single generation request
	GenerationTimeout time.Duration
	// FrameTimeout is the max time to wait for the next frame
	FrameTimeout time.Duration
	// ToolTimeout is the max time to wait for a tool execution result
	ToolTimeout time.Duration
}

// DefaultTimeoutConfig returns sensible defaults:
// - 5 minutes for generation
// - 30 seconds for each frame
// - 1 minute for tool execution
func DefaultTimeoutConfig() TimeoutConfig {
	return TimeoutConfig{
		GenerationTimeout: 5 * time.Minute,
		FrameTimeout:      30 * time.Second,
		ToolTimeout:       1 * time.Minute,
	}
}

// WithGenerationTimeout returns a context with a generation timeout applied.
func WithGenerationTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, timeout)
}

// TimeoutProvider wraps a Provider with generation timeout enforcement.
type TimeoutProvider struct {
	provider ryn.Provider
	timeout  time.Duration
}

// NewTimeoutProvider creates a Provider that enforces generation timeouts.
func NewTimeoutProvider(p ryn.Provider, timeout time.Duration) *TimeoutProvider {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &TimeoutProvider{provider: p, timeout: timeout}
}

// Generate implements Provider with timeout enforcement.
func (tp *TimeoutProvider) Generate(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
	tctx, cancel := context.WithTimeout(ctx, tp.timeout)

	stream, err := tp.provider.Generate(tctx, req)
	if err != nil {
		cancel()
		return nil, err
	}

	out, emitter := ryn.NewStream(32)
	go func() {
		defer cancel()
		defer emitter.Close()

		for stream.Next(tctx) {
			if err := emitter.Emit(tctx, stream.Frame()); err != nil {
				return
			}
		}
		if err := stream.Err(); err != nil {
			emitter.Error(err)
			return
		}
		if resp := stream.Response(); resp != nil {
			emitter.SetResponse(resp)
		}
		usage := stream.Usage()
		if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.TotalTokens > 0 {
			_ = emitter.Emit(tctx, ryn.UsageFrame(&usage))
		}
	}()

	return out, nil
}

// Composed combines multiple provider wrappers (timeout, tracing, retry).
// Useful for building a production-ready provider from building blocks.
// Apply order: retry (outermost) → timeout → tracing → base.
func Composed(p ryn.Provider, timeout time.Duration, retryConfig *RetryConfig) ryn.Provider {
	var composed ryn.Provider = p

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
