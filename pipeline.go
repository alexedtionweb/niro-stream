package ryn

import (
	"context"
	"sync"
)

// Pipeline chains Processors into a concurrent processing graph.
// Each Processor runs in its own goroutine with backpressure-aware
// channels connecting the stages.
//
//	input → [Proc A] → [Proc B] → [Proc C] → output
//	         goroutine   goroutine   goroutine
//
// Canceling the context tears down the entire pipeline.
// An error in any stage cancels all other stages.
type Pipeline struct {
	procs []Processor
	buf   int
}

// Pipe creates a Pipeline from the given Processors.
// Processors are executed in order: the output of each
// feeds into the input of the next.
func Pipe(procs ...Processor) *Pipeline {
	return &Pipeline{procs: procs, buf: 16}
}

// WithBuffer sets the channel buffer size between pipeline stages.
// Default is 16.
//
//   - 0: fully synchronous — minimum latency, maximum backpressure
//   - 16: good default for streaming
//   - 64+: high-throughput batch scenarios
func (p *Pipeline) WithBuffer(size int) *Pipeline {
	p.buf = size
	return p
}

// Run starts the pipeline. It reads Frames from the input Stream
// and returns a new Stream containing the output of the final Processor.
//
// Each Processor runs in its own goroutine. The pipeline self-destructs
// when the context is canceled or any processor returns an error.
//
// The returned Stream is safe to iterate immediately.
func (p *Pipeline) Run(ctx context.Context, in *Stream) *Stream {
	if len(p.procs) == 0 {
		return in
	}

	ctx, cancel := context.WithCancel(ctx)

	// Create inter-stage streams
	streams := make([]*Stream, len(p.procs))
	emitters := make([]*Emitter, len(p.procs))
	for i := range p.procs {
		streams[i], emitters[i] = NewStream(p.buf)
	}

	var wg sync.WaitGroup
	wg.Add(len(p.procs))

	for i, proc := range p.procs {
		input := in
		if i > 0 {
			input = streams[i-1]
		}
		output := emitters[i]

		go func() {
			defer wg.Done()
			defer output.Close()
			if err := proc.Process(ctx, input, output); err != nil {
				output.Error(err)
				cancel()
			}
		}()
	}

	go func() {
		wg.Wait()
		cancel()
	}()

	return streams[len(streams)-1]
}
