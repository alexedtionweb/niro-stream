// Package output provides stream-first routing of LLM output to typed sinks.
//
// Use [Route] or [RouteAgent] to tee a [niro.Stream] into a sink so that response
// text, thinking/reasoning, tool calls, and custom frames are dispatched as they
// arrive without buffering. The same stream is still forwarded to the caller so
// existing consumers see identical frames in the same order.
//
// # Agent identity
//
// [RouteAgent] injects the agent name into the context passed to every callback.
// Retrieve it with [AgentFromContext]. This allows sinks to attribute frames to the
// correct agent even when multiple agents stream in parallel through the same sink.
//
// # Sinks
//
// Set only the callback fields you need. Nil callbacks are skipped.
// Return a non-nil error from any callback to abort the stream.
//
// Convention for thinking: providers emit [niro.KindCustom] frames with
// Custom.Type "thinking" or "reasoning" and Data as string (or incremental).
// [Route] calls OnThinking for those; [Sink.OnText] is used for normal response text.
package output

import (
	"context"
	"fmt"

	"github.com/alexedtionweb/niro-stream"
)

type agentKey struct{}

// ContextWithAgent returns a child context carrying the agent name.
func ContextWithAgent(ctx context.Context, agent string) context.Context {
	return context.WithValue(ctx, agentKey{}, agent)
}

// AgentFromContext returns the agent name stored by [ContextWithAgent] (or "").
func AgentFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(agentKey{}).(string); ok {
		return v
	}
	return ""
}

// Sink receives LLM output as it streams. All fields are optional; nil callbacks are not called.
// Implementations should be fast and non-blocking; dispatch heavy work to another goroutine.
type Sink struct {
	// OnAgentStart is called once at the beginning of routing when an agent name is
	// provided via [RouteAgent]. Use it to log or display which agent is about to stream.
	OnAgentStart func(ctx context.Context, agent string) error

	// OnText is called for each [niro.KindText] frame (main assistant response for the user).
	OnText func(ctx context.Context, text string) error

	// OnThinking is called for [niro.KindCustom] frames with Type "thinking" or "reasoning".
	// Use to show extended thinking / reasoning in the UI.
	OnThinking func(ctx context.Context, text string) error

	// OnToolCall is called for each [niro.KindToolCall] frame (e.g. to show "Using tool X" in UI).
	OnToolCall func(ctx context.Context, call *niro.ToolCall) error

	// OnToolResult is called for each [niro.KindToolResult] frame (tool execution result). Use to show or log tool output.
	OnToolResult func(ctx context.Context, result *niro.ToolResult) error

	// OnCustom is called for other [niro.KindCustom] frames (e.g. handoff, summaries).
	OnCustom func(ctx context.Context, typ string, data any) error

	// OnEnd is called when the stream ends successfully (after last frame, before close).
	// Usage is the accumulated usage; resp may be nil when the provider didn't set metadata.
	OnEnd func(ctx context.Context, usage niro.Usage, resp *niro.ResponseMeta) error
}

// Route tees src into sink and returns a new stream that forwards all frames in order.
// Equivalent to RouteAgent(ctx, src, sink, "").
func Route(ctx context.Context, src *niro.Stream, sink *Sink) *niro.Stream {
	return RouteAgent(ctx, src, sink, "")
}

// RouteAgent tees src into sink with agent attribution.
// If agent is non-empty, the context passed to every callback carries the agent name
// (retrievable via [AgentFromContext]) and [Sink.OnAgentStart] is called before the
// first frame. Frames are dispatched to the sink's callbacks (by kind and, for
// KindCustom, by Type) as they are read; then the frame is emitted to the returned
// stream. Single-pass, no extra buffering — stream-first for minimal latency.
//
// If any sink callback returns a non-nil error, the returned stream is closed with that error.
// OnEnd is called when the source stream is exhausted without error (sink sees usage there).
func RouteAgent(ctx context.Context, src *niro.Stream, sink *Sink, agent string) *niro.Stream {
	if sink == nil {
		return src
	}

	cbCtx := ctx
	if agent != "" {
		cbCtx = ContextWithAgent(ctx, agent)
	}

	out, em := niro.NewStream(niro.DefaultStreamBuffer)
	go func() {
		defer em.Close()

		if agent != "" && sink.OnAgentStart != nil {
			if err := sink.OnAgentStart(cbCtx, agent); err != nil {
				em.Error(err)
				return
			}
		}

		for src.Next(ctx) {
			f := src.Frame()
			if err := dispatch(cbCtx, sink, f); err != nil {
				em.Error(err)
				return
			}
			if err := em.Emit(ctx, f); err != nil {
				return
			}
		}
		if err := src.Err(); err != nil {
			em.Error(err)
			return
		}
		resp := src.Response()
		if resp != nil {
			em.SetResponse(resp)
		}
		usage := src.Usage()
		if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.TotalTokens > 0 {
			uCopy := usage
			_ = em.Emit(ctx, niro.UsageFrame(&uCopy))
		}
		if sink.OnEnd != nil {
			if err := sink.OnEnd(cbCtx, usage, resp); err != nil {
				em.Error(err)
			}
		}
	}()
	return out
}

func dispatch(ctx context.Context, sink *Sink, f niro.Frame) error {
	switch f.Kind {
	case niro.KindText:
		if sink.OnText != nil && f.Text != "" {
			return sink.OnText(ctx, f.Text)
		}
	case niro.KindToolCall:
		if sink.OnToolCall != nil && f.Tool != nil {
			return sink.OnToolCall(ctx, f.Tool)
		}
	case niro.KindToolResult:
		if sink.OnToolResult != nil && f.Result != nil {
			return sink.OnToolResult(ctx, f.Result)
		}
	case niro.KindCustom:
		if f.Custom == nil {
			return nil
		}
		typ := f.Custom.Type
		data := f.Custom.Data
		switch typ {
		case niro.CustomThinking, niro.CustomReasoning:
			if sink.OnThinking != nil {
				s := dataString(data)
				if s != "" {
					return sink.OnThinking(ctx, s)
				}
			}
		default:
			if sink.OnCustom != nil {
				return sink.OnCustom(ctx, typ, data)
			}
		}
	}
	return nil
}

func dataString(data any) string {
	if data == nil {
		return ""
	}
	if s, ok := data.(string); ok {
		return s
	}
	return fmt.Sprint(data)
}
