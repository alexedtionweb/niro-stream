package ryn

import "context"

// Runtime manages the lifecycle of a Provider and an optional
// post-processing Pipeline. It is the top-level entry point for
// interactive sessions and agent loops.
//
// For simple use cases, calling Provider.Generate directly is fine.
// Runtime adds value when you want to compose a provider with
// a consistent processing pipeline.
//
// Future extensions (not yet implemented):
//   - Automatic tool execution loops
//   - Conversation state management
//   - Concurrent duplex streams
//   - Agent lifecycle hooks
type Runtime struct {
	provider Provider
	pipeline *Pipeline
}

// NewRuntime creates a Runtime with the given Provider.
func NewRuntime(p Provider) *Runtime {
	return &Runtime{provider: p}
}

// WithPipeline attaches a post-processing Pipeline to the Runtime.
// The pipeline is applied to every Generate call.
func (r *Runtime) WithPipeline(p *Pipeline) *Runtime {
	r.pipeline = p
	return r
}

// Generate sends a request to the provider and returns a stream
// of frames, optionally processed through the attached pipeline.
func (r *Runtime) Generate(ctx context.Context, req *Request) (*Stream, error) {
	stream, err := r.provider.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	if r.pipeline != nil {
		stream = r.pipeline.Run(ctx, stream)
	}
	return stream, nil
}
