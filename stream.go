package ryn

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned when emitting to a closed stream.
var ErrClosed = errors.New("ryn: stream closed")

// pipe is the shared state between a Stream and its Emitter.
type pipe struct {
	ch     chan Frame
	err    atomic.Pointer[error]
	once   sync.Once
	closed atomic.Bool
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
//
// Streams respect context cancellation and propagate errors
// from the writing side (Emitter).
type Stream struct {
	p     *pipe
	frame Frame
	err   error
}

// Next advances the Stream to the next Frame.
// Returns false when the stream is exhausted, an error occurs,
// or the context is canceled.
func (s *Stream) Next(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		s.err = ctx.Err()
		return false
	case f, ok := <-s.p.ch:
		if !ok {
			// Channel closed — check for writer error
			if ep := s.p.err.Load(); ep != nil {
				s.err = *ep
			}
			return false
		}
		s.frame = f
		return true
	}
}

// Frame returns the current Frame.
// Must only be called after Next returns true.
func (s *Stream) Frame() Frame { return s.frame }

// Err returns the first error encountered during iteration.
// Returns nil on clean end-of-stream.
func (s *Stream) Err() error { return s.err }

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
func (e *Emitter) Emit(ctx context.Context, f Frame) error {
	if e.p.closed.Load() {
		return ErrClosed
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case e.p.ch <- f:
		return nil
	}
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
		e.p.ch <- f // safe: buffer is exactly len(frames)
	}
	e.Close()
	return s
}

// Collect reads all frames from a Stream into a slice.
func Collect(ctx context.Context, s *Stream) ([]Frame, error) {
	var frames []Frame
	for s.Next(ctx) {
		frames = append(frames, s.Frame())
	}
	return frames, s.Err()
}

// CollectText reads all text frames and concatenates their content.
func CollectText(ctx context.Context, s *Stream) (string, error) {
	var buf []byte
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
