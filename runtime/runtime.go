// Package runtime manages the lifecycle of a Provider with optional hooks and
// a post-processing Pipeline. It is the top-level entry point for traced,
// observable LLM interactions.
package runtime

import (
	"context"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/hook"
	"github.com/alexedtionweb/niro-stream/pipe"
)

// Runtime manages the lifecycle of a Provider with hooks and
// an optional post-processing Pipeline.
//
// For simple use cases, calling Provider.Generate directly is fine.
// Runtime adds value when you want:
//   - Telemetry hooks on every generation
//   - Consistent post-processing pipeline
type Runtime struct {
	provider niro.Provider
	pipeline *pipe.Pipeline
	hook     hook.Hook
}

// New creates a Runtime with the given Provider.
func New(p niro.Provider) *Runtime {
	return &Runtime{provider: p}
}

// WithPipeline attaches a post-processing Pipeline to the Runtime.
func (r *Runtime) WithPipeline(p *pipe.Pipeline) *Runtime {
	r.pipeline = p
	return r
}

// WithHook attaches a telemetry/observability Hook.
// Use hook.Compose() to combine multiple hooks.
func (r *Runtime) WithHook(h hook.Hook) *Runtime {
	r.hook = h
	return r
}

// Generate sends a request to the provider and returns a stream
// of frames, optionally processed through the attached pipeline.
// Hooks are invoked at each stage.
func (r *Runtime) Generate(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
	start := time.Now()

	model := req.Model
	if model == "" {
		model = "(default)"
	}

	// Hook: start
	if r.hook != nil {
		ctx = r.hook.OnGenerateStart(ctx, hook.GenerateStartInfo{
			Model:    model,
			Messages: len(req.Messages),
			Tools:    len(req.Tools),
		})
	}

	stream, err := r.provider.Generate(ctx, req)
	if err != nil {
		if r.hook != nil {
			r.hook.OnError(ctx, err)
			r.hook.OnGenerateEnd(ctx, hook.GenerateEndInfo{
				Model:    model,
				Duration: time.Since(start),
				Error:    err,
			})
		}
		return nil, err
	}

	if r.pipeline != nil {
		stream = r.pipeline.Run(ctx, stream)
	}

	// If we have a hook, wrap the stream to intercept frames and
	// fire OnGenerateEnd when the stream is exhausted.
	if r.hook != nil {
		stream = r.wrapStream(ctx, stream, model, start)
	}

	return stream, nil
}

// wrapStream interposes a hook between the provider stream and the consumer.
func (r *Runtime) wrapStream(ctx context.Context, src *niro.Stream, model string, start time.Time) *niro.Stream {
	out, emitter := niro.NewStream(32)

	go func() {
		defer emitter.Close()

		for src.Next(ctx) {
			f := src.Frame()

			// Hook per-frame with wall-clock elapsed since generation start.
			// Receivers can extract TTFT from the first KindText frame.
			if err := r.hook.OnFrame(ctx, f, time.Since(start)); err != nil {
				emitter.Error(err)
				r.hook.OnError(ctx, err)
				return
			}

			if err := emitter.Emit(ctx, f); err != nil {
				return
			}
		}

		// Always capture usage — even on error, partial usage may exist.
		usage := src.Usage()
		resp := src.Response()

		if err := src.Err(); err != nil {
			emitter.Error(err)
			r.hook.OnError(ctx, err)
			r.hook.OnGenerateEnd(ctx, hook.GenerateEndInfo{
				Model:    model,
				Usage:    usage,
				Duration: time.Since(start),
				Error:    err,
			})
			return
		}

		// Propagate response metadata
		if resp != nil {
			emitter.SetResponse(resp)
		}

		finishReason := ""
		responseID := ""
		if resp != nil {
			finishReason = resp.FinishReason
			responseID = resp.ID
			usage.Add(&resp.Usage)
		}

		r.hook.OnGenerateEnd(ctx, hook.GenerateEndInfo{
			Model:        model,
			Usage:        usage,
			FinishReason: finishReason,
			Duration:     time.Since(start),
			ResponseID:   responseID,
		})
	}()

	return out
}
