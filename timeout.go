package niro

import (
	"context"
	"time"
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

// TelephonyTimeoutConfig returns timeouts tuned for real-time voice pipelines
// where the full response must fit inside a conversational turn:
//
//   - GenerationTimeout: 8 s  — total budget for a single voice turn
//   - FrameTimeout:      300 ms — barge-in detection window; stalled tokens
//     abort the stream before the user notices a freeze
//   - ToolTimeout:       5 s  — tools must resolve within the voice-turn budget
//
// These values are intentionally conservative.  Adjust per deployment after
// measuring p99 TTFT and tool-execution latencies in production.
func TelephonyTimeoutConfig() TimeoutConfig {
	return TimeoutConfig{
		GenerationTimeout: 8 * time.Second,
		FrameTimeout:      300 * time.Millisecond,
		ToolTimeout:       5 * time.Second,
	}
}

// WithGenerationTimeout returns a context with a generation timeout applied.
func WithGenerationTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, timeout)
}

// TimeoutProvider wraps a Provider with generation timeout enforcement.
//
// The timeout covers the entire generation lifecycle: from the initial
// API call through streaming until the last frame is consumed.
type TimeoutProvider struct {
	provider Provider
	timeout  time.Duration
}

// NewTimeoutProvider creates a Provider that enforces generation timeouts.
func NewTimeoutProvider(p Provider, timeout time.Duration) *TimeoutProvider {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &TimeoutProvider{provider: p, timeout: timeout}
}

// Generate implements Provider with timeout enforcement.
//
// The timeout context is propagated into the underlying stream so that
// it remains active for the full duration of stream consumption — not
// just the initial Generate call.
func (tp *TimeoutProvider) Generate(ctx context.Context, req *Request) (*Stream, error) {
	tctx, cancel := context.WithTimeout(ctx, tp.timeout)

	stream, err := tp.provider.Generate(tctx, req)
	if err != nil {
		cancel()
		return nil, err
	}

	// Wrap the stream: forward all frames under the timeout context,
	// and call cancel() when the stream is fully consumed.
	out, emitter := NewStream(32)
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
		// Propagate response metadata
		if resp := stream.Response(); resp != nil {
			emitter.SetResponse(resp)
		}
		// Re-emit accumulated usage so the outer stream accumulates it
		usage := stream.Usage()
		if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.TotalTokens > 0 {
			_ = emitter.Emit(tctx, UsageFrame(&usage))
		}
	}()

	return out, nil
}
