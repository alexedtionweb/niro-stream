package ryn

import "context"

// Processor transforms Frames from an input Stream to an output Emitter.
// It is the fundamental unit of composition in a Ryn pipeline.
//
// Contracts:
//   - Process must not close the Emitter — the Pipeline manages that.
//   - Process should return when ctx is canceled or the input stream ends.
//   - Errors returned from Process are propagated to the output stream.
//   - Process runs in its own goroutine when used in a Pipeline.
type Processor interface {
	Process(ctx context.Context, in *Stream, out *Emitter) error
}

// ProcessorFunc adapts a plain function to the Processor interface.
type ProcessorFunc func(ctx context.Context, in *Stream, out *Emitter) error

func (f ProcessorFunc) Process(ctx context.Context, in *Stream, out *Emitter) error {
	return f(ctx, in, out)
}

// --- Built-in Processors ---

// PassThrough creates a Processor that forwards all frames unchanged.
func PassThrough() Processor {
	return ProcessorFunc(func(ctx context.Context, in *Stream, out *Emitter) error {
		return Forward(ctx, in, out)
	})
}

// Filter creates a Processor that only forwards frames matching the predicate.
func Filter(fn func(Frame) bool) Processor {
	return ProcessorFunc(func(ctx context.Context, in *Stream, out *Emitter) error {
		for in.Next(ctx) {
			f := in.Frame()
			if fn(f) {
				if err := out.Emit(ctx, f); err != nil {
					return err
				}
			}
		}
		return in.Err()
	})
}

// Map creates a Processor that transforms each frame through fn.
func Map(fn func(Frame) Frame) Processor {
	return ProcessorFunc(func(ctx context.Context, in *Stream, out *Emitter) error {
		for in.Next(ctx) {
			if err := out.Emit(ctx, fn(in.Frame())); err != nil {
				return err
			}
		}
		return in.Err()
	})
}

// Tap creates a Processor that calls fn for each frame as a side effect
// without modifying the stream. Useful for logging, metrics, or debugging.
func Tap(fn func(Frame)) Processor {
	return ProcessorFunc(func(ctx context.Context, in *Stream, out *Emitter) error {
		for in.Next(ctx) {
			f := in.Frame()
			fn(f)
			if err := out.Emit(ctx, f); err != nil {
				return err
			}
		}
		return in.Err()
	})
}

// TextOnly creates a Processor that only forwards KindText frames.
func TextOnly() Processor {
	return Filter(func(f Frame) bool { return f.Kind == KindText })
}

// Accumulate creates a Processor that collects all text into a single
// frame emitted at the end of the stream. Useful for converting a
// token stream into a complete response.
func Accumulate() Processor {
	return ProcessorFunc(func(ctx context.Context, in *Stream, out *Emitter) error {
		var buf []byte
		for in.Next(ctx) {
			f := in.Frame()
			if f.Kind == KindText {
				buf = append(buf, f.Text...)
			} else {
				// Non-text frames pass through immediately
				if err := out.Emit(ctx, f); err != nil {
					return err
				}
			}
		}
		if err := in.Err(); err != nil {
			return err
		}
		if len(buf) > 0 {
			return out.Emit(ctx, TextFrame(string(buf)))
		}
		return nil
	})
}
