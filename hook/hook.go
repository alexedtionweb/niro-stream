// Package hook provides the Hook interface for observing LLM generation
// lifecycle events: start, end, per-frame, tool calls, and errors.
package hook

import (
	"context"
	"time"

	"ryn.dev/ryn"
)

// Hook provides observability into Ryn operations.
// Implement this interface for telemetry, logging, cost tracking,
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
	// This is the per-token hook — keep it extremely fast.
	// A nil return is fine; return a non-nil error to abort the stream.
	OnFrame(ctx context.Context, f ryn.Frame) error

	// OnToolCall is called when a tool call is about to be executed.
	OnToolCall(ctx context.Context, call ryn.ToolCall)

	// OnToolResult is called when a tool call completes.
	OnToolResult(ctx context.Context, result ryn.ToolResult, elapsed time.Duration)

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
	Usage        ryn.Usage     // Token usage
	Cost         ryn.Cost      // Cost information (tokens × model pricing)
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

func (m *multiHook) OnFrame(ctx context.Context, f ryn.Frame) error {
	var firstErr error
	for _, h := range m.hooks {
		if err := h.OnFrame(ctx, f); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *multiHook) OnToolCall(ctx context.Context, call ryn.ToolCall) {
	for _, h := range m.hooks {
		h.OnToolCall(ctx, call)
	}
}

func (m *multiHook) OnToolResult(ctx context.Context, result ryn.ToolResult, elapsed time.Duration) {
	for _, h := range m.hooks {
		h.OnToolResult(ctx, result, elapsed)
	}
}

func (m *multiHook) OnError(ctx context.Context, err error) {
	for _, h := range m.hooks {
		h.OnError(ctx, err)
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
func (NoOpHook) OnGenerateEnd(context.Context, GenerateEndInfo)              {}
func (NoOpHook) OnFrame(context.Context, ryn.Frame) error                    { return nil }
func (NoOpHook) OnToolCall(context.Context, ryn.ToolCall)                    {}
func (NoOpHook) OnToolResult(context.Context, ryn.ToolResult, time.Duration) {}
func (NoOpHook) OnError(context.Context, error)                              {}

// Verify NoOpHook implements Hook.
var _ Hook = NoOpHook{}
