package ryn

import (
	"context"
	"time"
)

// Runtime manages the lifecycle of a Provider with hooks and
// an optional post-processing Pipeline. It is the top-level
// entry point for traced, observable LLM interactions.
//
// For simple use cases, calling Provider.Generate directly is fine.
// Runtime adds value when you want:
//   - Telemetry hooks on every generation
//   - Consistent post-processing pipeline
//   - Future: automatic tool execution loops
type Runtime struct {
	provider Provider
	pipeline *Pipeline
	hook     Hook
}

// NewRuntime creates a Runtime with the given Provider.
func NewRuntime(p Provider) *Runtime {
	return &Runtime{provider: p}
}

// WithPipeline attaches a post-processing Pipeline to the Runtime.
func (r *Runtime) WithPipeline(p *Pipeline) *Runtime {
	r.pipeline = p
	return r
}

// WithHook attaches a telemetry/observability Hook.
// Use Hooks() to compose multiple hooks.
func (r *Runtime) WithHook(h Hook) *Runtime {
	r.hook = h
	return r
}

// Generate sends a request to the provider and returns a stream
// of frames, optionally processed through the attached pipeline.
// Hooks are invoked at each stage.
func (r *Runtime) Generate(ctx context.Context, req *Request) (*Stream, error) {
	start := time.Now()

	model := req.Model
	if model == "" {
		model = "(default)"
	}

	// Hook: start
	if r.hook != nil {
		ctx = r.hook.OnGenerateStart(ctx, GenerateStartInfo{
			Model:    model,
			Messages: len(req.Messages),
			Tools:    len(req.Tools),
		})
	}

	stream, err := r.provider.Generate(ctx, req)
	if err != nil {
		if r.hook != nil {
			r.hook.OnError(ctx, err)
			r.hook.OnGenerateEnd(ctx, GenerateEndInfo{
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
func (r *Runtime) wrapStream(ctx context.Context, src *Stream, model string, start time.Time) *Stream {
	out, emitter := NewStream(32)

	go func() {
		defer emitter.Close()
		var usage Usage

		for src.Next(ctx) {
			f := src.Frame()

			// Hook per-frame
			if err := r.hook.OnFrame(ctx, f); err != nil {
				emitter.Error(err)
				r.hook.OnError(ctx, err)
				return
			}

			if err := emitter.Emit(ctx, f); err != nil {
				return
			}
		}

		if err := src.Err(); err != nil {
			emitter.Error(err)
			r.hook.OnError(ctx, err)
			r.hook.OnGenerateEnd(ctx, GenerateEndInfo{
				Model:    model,
				Usage:    usage,
				Duration: time.Since(start),
				Error:    err,
			})
			return
		}

		// Propagate response metadata
		usage = src.Usage()
		resp := src.Response()
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

		r.hook.OnGenerateEnd(ctx, GenerateEndInfo{
			Model:        model,
			Usage:        usage,
			FinishReason: finishReason,
			Duration:     time.Since(start),
			ResponseID:   responseID,
		})
	}()

	return out
}
