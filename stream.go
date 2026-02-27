package ryn

import (
	"context"
	"sync"
	"sync/atomic"
)

// pipe is the shared state between a Stream and its Emitter.
type pipe struct {
	ch     chan Frame
	err    atomic.Pointer[error]
	once   sync.Once
	closed atomic.Bool
	resp   atomic.Pointer[ResponseMeta]
}

// Stream reads Frames from a pipeline stage.
//
// Stream follows the bufio.Scanner iteration pattern:
//
//	for stream.Next(ctx) {
//	    f := stream.Frame()
//	    // process f
//	}
//	if err := stream.Err(); err != nil {
//	    // handle error
//	}
//	usage := stream.Usage()
//
// Streams respect context cancellation and propagate errors
// from the writing side (Emitter). Usage data is accumulated
// automatically from KindUsage frames.
type Stream struct {
	p     *pipe
	frame Frame
	err   error
	usage Usage
}

// Next advances the Stream to the next Frame.
// Returns false when the stream is exhausted, an error occurs,
// or the context is canceled.
//
// KindUsage frames are consumed automatically and accumulated
// in the Usage — they are not returned to the caller.
func (s *Stream) Next(ctx context.Context) bool {
	for {
		select {
		case <-ctx.Done():
			s.err = ctx.Err()
			return false
		case f, ok := <-s.p.ch:
			if !ok {
				if ep := s.p.err.Load(); ep != nil {
					s.err = *ep
				}
				return false
			}
			// Silently accumulate usage frames
			if f.Kind == KindUsage && f.Usage != nil {
				s.usage.Add(f.Usage)
				continue
			}
			s.frame = f
			return true
		}
	}
}

// Frame returns the current Frame.
// Must only be called after Next returns true.
func (s *Stream) Frame() Frame { return s.frame }

// Err returns the first error encountered during iteration.
// Returns nil on clean end-of-stream.
func (s *Stream) Err() error { return s.err }

// Usage returns the accumulated token usage.
// Fully populated after the stream is exhausted.
func (s *Stream) Usage() Usage { return s.usage }

// Response returns provider metadata set by the Emitter.
// Available after the stream is fully consumed.
func (s *Stream) Response() *ResponseMeta {
	return s.p.resp.Load()
}

// Chan exposes the underlying channel for use in select statements.
// Advanced use only — prefer Next for standard iteration.
func (s *Stream) Chan() <-chan Frame { return s.p.ch }

// Emitter writes Frames into a Stream.
// It is the write half of a Stream pipe.
//
// Contract: do not call Emit after Close or Error.
// The Pipeline ensures this automatically for Processors.
type Emitter struct {
	p *pipe
}

// Emit sends a Frame into the stream.
// Blocks if the stream buffer is full (backpressure).
// Returns an error if the context is canceled or the stream is closed.
//
// Emit is safe to call concurrently with Close — a concurrent close
// returns ErrClosed instead of panicking.
func (e *Emitter) Emit(ctx context.Context, f Frame) (err error) {
	if e.p.closed.Load() {
		return ErrClosed
	}
	// Recover from send-on-closed-channel if Close() races between
	// the check above and the channel send below (TOCTOU).
	defer func() {
		if r := recover(); r != nil {
			err = ErrClosed
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case e.p.ch <- f:
		return nil
	}
}

// SetResponse stores provider metadata on the stream.
// Call this before Close, typically after all frames are emitted.
func (e *Emitter) SetResponse(meta *ResponseMeta) {
	e.p.resp.Store(meta)
}

// Error sets an error on the stream and closes it.
// The error is visible to the reader via Stream.Err().
func (e *Emitter) Error(err error) {
	e.p.err.Store(&err)
	e.Close()
}

// Close closes the stream. Safe to call multiple times.
func (e *Emitter) Close() {
	e.p.once.Do(func() {
		e.p.closed.Store(true)
		close(e.p.ch)
	})
}

// --- Constructors ---

// NewStream creates a paired Stream and Emitter.
//
// bufSize controls the channel buffer size between writer and reader.
// A bufSize of 0 means fully synchronous (unbuffered) — the writer
// blocks until the reader consumes each frame. Larger values allow
// the writer to get ahead, trading memory for throughput.
//
// Typical values:
//   - 0:  telephony / real-time (minimal latency)
//   - 16: general streaming (good default)
//   - 64: batch-style processing
func NewStream(bufSize int) (*Stream, *Emitter) {
	p := &pipe{ch: make(chan Frame, bufSize)}
	return &Stream{p: p}, &Emitter{p: p}
}

// --- Utilities ---

// StreamFromSlice creates a Stream pre-loaded with the given frames.
// The stream is immediately closed after all frames are buffered.
// Useful for testing and providing static input.
func StreamFromSlice(frames []Frame) *Stream {
	s, e := NewStream(len(frames))
	for _, f := range frames {
		e.p.ch <- f
	}
	e.Close()
	return s
}

// Collect reads all frames from a Stream into a slice.
// Pre-allocates capacity when possible to minimize slice growth.
func Collect(ctx context.Context, s *Stream) ([]Frame, error) {
	// Pre-allocate with reasonable capacity to reduce grow-copy overhead
	frames := make([]Frame, 0, 64)
	for s.Next(ctx) {
		frames = append(frames, s.Frame())
	}
	return frames, s.Err()
}

// CollectText reads all text frames and concatenates their content.
// Uses a byte buffer with pre-allocated capacity.
func CollectText(ctx context.Context, s *Stream) (string, error) {
	buf := make([]byte, 0, 4096) // typical LLM response < 4KB
	for s.Next(ctx) {
		f := s.Frame()
		if f.Kind == KindText {
			buf = append(buf, f.Text...)
		}
	}
	return string(buf), s.Err()
}

// Forward reads all frames from src and emits them to dst.
// Useful for connecting streams in custom Processors.
func Forward(ctx context.Context, src *Stream, dst *Emitter) error {
	for src.Next(ctx) {
		if err := dst.Emit(ctx, src.Frame()); err != nil {
			return err
		}
	}
	return src.Err()
}
