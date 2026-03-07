// Package hook provides the Hook interface for observing LLM generation
// lifecycle events: start, end, per-frame, tool calls, and errors.
package hook

import (
	"context"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

// Hook provides observability into Niro operations.
// Implement this interface for telemetry, logging,
// or integration with platforms like Langfuse, Datadog, or OpenTelemetry.
//
// All methods are called synchronously in the hot path.
// Implementations MUST be fast and non-blocking.
// Heavy work (network I/O, persistence) should be dispatched
// to a background goroutine.
//
// A zero-value Hook (nil) is safe — the runtime checks before calling.
type Hook interface {
	// OnGenerateStart is called before a Provider.Generate request.
	// The returned context is passed to the provider — use it to
	// inject trace IDs, span contexts, or request-scoped values.
	OnGenerateStart(ctx context.Context, info GenerateStartInfo) context.Context

	// OnGenerateEnd is called after a generation stream is fully consumed.
	// It receives the final usage, response metadata, and any error.
	OnGenerateEnd(ctx context.Context, info GenerateEndInfo)

	// OnFrame is called for each frame emitted by the provider.
	// elapsed is the wall-clock duration since OnGenerateStart. Use it to
	// measure time-to-first-token (TTFT) and inter-token jitter — critical
	// metrics for real-time and telephony applications.
	// This is the per-token hook — keep it extremely fast.
	// A nil return is fine; return a non-nil error to abort the stream.
	OnFrame(ctx context.Context, f niro.Frame, elapsed time.Duration) error

	// OnToolCall is called when a tool call is about to be executed.
	OnToolCall(ctx context.Context, call niro.ToolCall)

	// OnToolResult is called when a tool call completes.
	OnToolResult(ctx context.Context, result niro.ToolResult, elapsed time.Duration)

	// OnError is called when an error occurs at any stage.
	OnError(ctx context.Context, err error)
}

// GenerateStartInfo contains metadata about an incoming generation request.
type GenerateStartInfo struct {
	Provider   string            // Provider name (e.g. "openai", "anthropic")
	Model      string            // Requested model
	Messages   int               // Number of messages
	Tools      int               // Number of tools
	RequestID  string            // Unique request ID for tracing
	FunctionID string            // Caller-assigned ID for tracing
	Metadata   map[string]string // Arbitrary metadata from the caller
}

// GenerateEndInfo contains metadata about a completed generation.
type GenerateEndInfo struct {
	Provider     string        // Provider name
	Model        string        // Model actually used
	RequestID    string        // Unique request ID for tracing
	Usage        niro.Usage    // Token usage
	FinishReason string        // Why generation stopped
	Duration     time.Duration // Wall-clock duration
	ResponseID   string        // Provider-assigned response ID
	Error        error         // Non-nil if generation failed
}

// --- Hook utilities ---

// Compose combines multiple Hooks into one.
// Hooks are called in order. If any OnFrame returns an error,
// remaining hooks are still called but the error propagates.
func Compose(hooks ...Hook) Hook {
	// Filter nils
	var valid []Hook
	for _, h := range hooks {
		if h != nil {
			valid = append(valid, h)
		}
	}
	switch len(valid) {
	case 0:
		return nil
	case 1:
		return valid[0]
	default:
		return &multiHook{hooks: valid}
	}
}

type multiHook struct {
	hooks []Hook
}

func (m *multiHook) OnGenerateStart(ctx context.Context, info GenerateStartInfo) context.Context {
	for _, h := range m.hooks {
		ctx = h.OnGenerateStart(ctx, info)
	}
	return ctx
}

func (m *multiHook) OnGenerateEnd(ctx context.Context, info GenerateEndInfo) {
	for _, h := range m.hooks {
		h.OnGenerateEnd(ctx, info)
	}
}

func (m *multiHook) OnFrame(ctx context.Context, f niro.Frame, elapsed time.Duration) error {
	var firstErr error
	for _, h := range m.hooks {
		if err := h.OnFrame(ctx, f, elapsed); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *multiHook) OnToolCall(ctx context.Context, call niro.ToolCall) {
	for _, h := range m.hooks {
		h.OnToolCall(ctx, call)
	}
}

func (m *multiHook) OnToolResult(ctx context.Context, result niro.ToolResult, elapsed time.Duration) {
	for _, h := range m.hooks {
		h.OnToolResult(ctx, result, elapsed)
	}
}

func (m *multiHook) OnError(ctx context.Context, err error) {
	for _, h := range m.hooks {
		h.OnError(ctx, err)
	}
}

// --- Stream wrapping ---

// WrapStream interposes a Hook between a provider stream and the consumer.
// It creates a goroutine that reads from src, fires OnFrame (and OnToolCall /
// OnToolResult for the corresponding frame kinds) for each frame, and emits
// the frame to the returned stream. When the source is exhausted it fires
// OnGenerateEnd with usage and duration. On any error it fires OnError.
//
// Both runtime.Runtime and dsl.Runner use this to get consistent hook
// coverage from a single implementation.
func WrapStream(ctx context.Context, src *niro.Stream, h Hook, model string, start time.Time) *niro.Stream {
	if h == nil {
		return src
	}
	out, emitter := niro.NewStream(niro.DefaultStreamBuffer)

	go func() {
		defer emitter.Close()

		for src.Next(ctx) {
			f := src.Frame()

			if err := h.OnFrame(ctx, f, time.Since(start)); err != nil {
				emitter.Error(err)
				h.OnError(ctx, err)
				return
			}

			if f.Kind == niro.KindToolCall && f.Tool != nil {
				h.OnToolCall(ctx, *f.Tool)
			}
			if f.Kind == niro.KindToolResult && f.Result != nil {
				h.OnToolResult(ctx, *f.Result, time.Since(start))
			}

			if err := emitter.Emit(ctx, f); err != nil {
				return
			}
		}

		usage := src.Usage()
		resp := src.Response()

		if err := src.Err(); err != nil {
			emitter.Error(err)
			h.OnError(ctx, err)
			h.OnGenerateEnd(ctx, GenerateEndInfo{
				Model:    model,
				Usage:    usage,
				Duration: time.Since(start),
				Error:    err,
			})
			return
		}

		if resp != nil {
			emitter.SetResponse(resp)
		}

		finalModel := model
		finishReason := ""
		responseID := ""
		if resp != nil {
			if resp.Model != "" {
				finalModel = resp.Model
			}
			finishReason = resp.FinishReason
			responseID = resp.ID
			mergeUsage(&usage, &resp.Usage)
		}

		h.OnGenerateEnd(ctx, GenerateEndInfo{
			Model:        finalModel,
			Usage:        usage,
			FinishReason: finishReason,
			Duration:     time.Since(start),
			ResponseID:   responseID,
		})
	}()

	return out
}

func mergeUsage(dst *niro.Usage, fallback *niro.Usage) {
	if dst == nil || fallback == nil {
		return
	}
	if dst.InputTokens == 0 {
		dst.InputTokens = fallback.InputTokens
	}
	if dst.OutputTokens == 0 {
		dst.OutputTokens = fallback.OutputTokens
	}
	if dst.TotalTokens == 0 {
		dst.TotalTokens = fallback.TotalTokens
	}
	if len(fallback.Detail) == 0 {
		return
	}
	if dst.Detail == nil {
		dst.Detail = make(map[string]int, len(fallback.Detail))
	}
	for key, value := range fallback.Detail {
		if _, exists := dst.Detail[key]; !exists {
			dst.Detail[key] = value
		}
	}
}

// --- NoOpHook ---

// NoOpHook is a Hook that does nothing.
// Embed it in your hook struct to only override the methods you need:
//
//	type MyHook struct { hook.NoOpHook }
//	func (h *MyHook) OnGenerateEnd(ctx context.Context, info hook.GenerateEndInfo) {
//	    log.Printf("model=%s tokens=%d", info.Model, info.Usage.TotalTokens)
//	}
type NoOpHook struct{}

func (NoOpHook) OnGenerateStart(ctx context.Context, _ GenerateStartInfo) context.Context {
	return ctx
}
func (NoOpHook) OnGenerateEnd(context.Context, GenerateEndInfo)               {}
func (NoOpHook) OnFrame(context.Context, niro.Frame, time.Duration) error     { return nil }
func (NoOpHook) OnToolCall(context.Context, niro.ToolCall)                    {}
func (NoOpHook) OnToolResult(context.Context, niro.ToolResult, time.Duration) {}
func (NoOpHook) OnError(context.Context, error)                               {}

// Verify NoOpHook implements Hook.
var _ Hook = NoOpHook{}
