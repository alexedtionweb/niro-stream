// Package output provides stream-first routing of LLM output to typed sinks.
//
// Use [Route] to tee a [niro.Stream] into one or more sinks so that response text,
// thinking/reasoning, tool calls, and custom frames are dispatched as they arrive
// without buffering. The same stream is still forwarded to the caller so existing
// consumers see identical frames in the same order.
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

// Sink receives LLM output as it streams. All fields are optional; nil callbacks are not called.
// Implementations should be fast and non-blocking; dispatch heavy work to another goroutine.
type Sink struct {
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
	// Usage is the accumulated usage from the stream.
	OnEnd func(ctx context.Context, usage niro.Usage) error
}

// Route tees src into sink and returns a new stream that forwards all frames in order.
// Frames are dispatched to the sink's callbacks (by kind and, for KindCustom, by Type)
// as they are read; then the frame is emitted to the returned stream. Single-pass,
// no extra buffering — stream-first for minimal latency.
//
// If any sink callback returns a non-nil error, the returned stream is closed with that error.
// OnEnd is called when the source stream is exhausted without error (sink sees usage there).
func Route(ctx context.Context, src *niro.Stream, sink *Sink) *niro.Stream {
	if sink == nil {
		return src
	}
	out, em := niro.NewStream(32)
	go func() {
		defer em.Close()
		for src.Next(ctx) {
			f := src.Frame()
			if err := dispatch(ctx, sink, f); err != nil {
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
		if resp := src.Response(); resp != nil {
			em.SetResponse(resp)
		}
		usage := src.Usage()
		if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.TotalTokens > 0 {
			uCopy := usage
			_ = em.Emit(ctx, niro.UsageFrame(&uCopy))
		}
		if sink.OnEnd != nil {
			if err := sink.OnEnd(ctx, usage); err != nil {
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
		case "thinking", "reasoning":
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
