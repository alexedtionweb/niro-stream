// Package orchestrate provides concurrent workflow primitives:
// Fan (parallel merge), Race (first wins), and Sequence (chained).
package orchestrate

import (
	"context"
	"sync"

	"ryn.dev/ryn"
)

// Fan runs multiple providers or requests in parallel and merges
// results into a single stream. Each generation runs in its own
// goroutine. Frames are interleaved in arrival order.
//
// Use cases:
//   - Parallel tool calls
//   - Multi-model ensembles
//   - Concurrent sub-agent invocations
//   - Scatter-gather patterns
//
// All streams are consumed. If any generation fails, the error
// is propagated but remaining streams continue until done or
// the context is canceled.
func Fan(ctx context.Context, fns ...func(context.Context) (*ryn.Stream, error)) *ryn.Stream {
	out, emitter := ryn.NewStream(32)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	wg.Add(len(fns))

	for _, fn := range fns {
		go func() {
			defer wg.Done()
			s, err := fn(ctx)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			for s.Next(ctx) {
				if err := emitter.Emit(ctx, s.Frame()); err != nil {
					return
				}
			}
			if err := s.Err(); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}()
	}

	go func() {
		wg.Wait()
		if firstErr != nil {
			emitter.Error(firstErr)
		} else {
			emitter.Close()
		}
	}()

	return out
}

// Race runs multiple generations in parallel and returns the
// first complete text response. All other generations are canceled.
//
// Use cases:
//   - Latency hedging (send to multiple providers, take fastest)
//   - Speculative execution
//
// Returns the collected text from the winning stream and its usage.
func Race(ctx context.Context, fns ...func(context.Context) (*ryn.Stream, error)) (string, ryn.Usage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		text  string
		usage ryn.Usage
		err   error
	}

	ch := make(chan result, len(fns))

	for _, fn := range fns {
		go func() {
			s, err := fn(ctx)
			if err != nil {
				ch <- result{err: err}
				return
			}
			text, err := ryn.CollectText(ctx, s)
			ch <- result{text: text, usage: s.Usage(), err: err}
		}()
	}

	// Wait for first success
	var lastErr error
	for range fns {
		r := <-ch
		if r.err != nil {
			lastErr = r.err
			continue
		}
		cancel() // stop others
		return r.text, r.usage, nil
	}
	return "", ryn.Usage{}, lastErr
}

// Sequence runs multiple generation functions sequentially,
// where each function receives the collected text output of
// the previous one. The final stream contains only the last
// generation's output.
//
// This is a building block for chained LLM calls where each
// step transforms or refines the previous output.
func Sequence(ctx context.Context, fns ...func(ctx context.Context, input string) (*ryn.Stream, error)) (*ryn.Stream, error) {
	if len(fns) == 0 {
		return ryn.StreamFromSlice(nil), nil
	}

	input := ""
	for i, fn := range fns {
		stream, err := fn(ctx, input)
		if err != nil {
			return nil, err
		}

		// Last stage: return the stream directly (caller consumes)
		if i == len(fns)-1 {
			return stream, nil
		}

		// Intermediate stages: collect to feed into next
		text, err := ryn.CollectText(ctx, stream)
		if err != nil {
			return nil, err
		}
		input = text
	}

	// unreachable
	return ryn.StreamFromSlice(nil), nil
}
