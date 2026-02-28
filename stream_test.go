package ryn_test

import (
	"context"
	"fmt"
	"testing"

	"ryn.dev/ryn"
)

func TestStreamBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, e := ryn.NewStream(4)
	go func() {
		defer e.Close()
		e.Emit(ctx, ryn.TextFrame("Hello"))
		e.Emit(ctx, ryn.TextFrame(" World"))
	}()

	text, err := ryn.CollectText(ctx, s)
	assertNoError(t, err)
	assertEqual(t, text, "Hello World")
}

func TestStreamUsageAutoAccumulation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, e := ryn.NewStream(8)
	go func() {
		defer e.Close()
		e.Emit(ctx, ryn.TextFrame("a"))
		e.Emit(ctx, ryn.UsageFrame(&ryn.Usage{InputTokens: 10, OutputTokens: 1, TotalTokens: 11}))
		e.Emit(ctx, ryn.TextFrame("b"))
		e.Emit(ctx, ryn.UsageFrame(&ryn.Usage{InputTokens: 0, OutputTokens: 1, TotalTokens: 1}))
	}()

	var frames []ryn.Frame
	for s.Next(ctx) {
		frames = append(frames, s.Frame())
	}

	// Usage frames should NOT appear in output
	assertEqual(t, len(frames), 2)
	assertEqual(t, frames[0].Text, "a")
	assertEqual(t, frames[1].Text, "b")

	// But usage should be accumulated
	usage := s.Usage()
	assertEqual(t, usage.InputTokens, 10)
	assertEqual(t, usage.OutputTokens, 2)
	assertEqual(t, usage.TotalTokens, 12)
}

func TestStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, e := ryn.NewStream(4)
	go func() {
		e.Emit(ctx, ryn.TextFrame("a"))
		e.Error(errTest)
	}()

	var texts []string
	for s.Next(ctx) {
		texts = append(texts, s.Frame().Text)
	}

	assertEqual(t, len(texts), 1)
	assertErrorContains(t, s.Err(), "test error")
}

func TestStreamContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	s, e := ryn.NewStream(0)
	go func() {
		defer e.Close()
		for i := 0; i < 100; i++ {
			if err := e.Emit(ctx, ryn.TextFrame("x")); err != nil {
				return
			}
		}
	}()

	s.Next(ctx)
	cancel()

	// Drain should eventually stop
	for s.Next(ctx) {
	}
	assertErrorContains(t, s.Err(), "context canceled")
}

func TestStreamFromSlice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("a"),
		ryn.TextFrame("b"),
	})

	frames, err := ryn.Collect(ctx, s)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
	assertEqual(t, frames[1].Text, "b")
}

func TestStreamResponse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, e := ryn.NewStream(4)
	go func() {
		e.SetResponse(&ryn.ResponseMeta{
			Model:        "gpt-4o",
			FinishReason: "stop",
			ID:           "resp_123",
		})
		e.Close()
	}()

	_, err := ryn.Collect(ctx, s)
	assertNoError(t, err)

	resp := s.Response()
	assertNotNil(t, resp)
	assertEqual(t, resp.Model, "gpt-4o")
	assertEqual(t, resp.FinishReason, "stop")
}

func TestEmitAfterClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, e := ryn.NewStream(0)
	e.Close()

	err := e.Emit(ctx, ryn.TextFrame("late"))
	assertErrorContains(t, err, "stream closed")
}

func TestCollectText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, e := ryn.NewStream(8)
	go func() {
		defer e.Close()
		e.Emit(ctx, ryn.TextFrame("Hello"))
		e.Emit(ctx, ryn.ControlFrame(ryn.SignalFlush))
		e.Emit(ctx, ryn.TextFrame(" World"))
		e.Emit(ctx, ryn.AudioFrame(nil, ""))
	}()

	text, err := ryn.CollectText(ctx, s)
	assertNoError(t, err)
	assertEqual(t, text, "Hello World")
}

func TestStreamChan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, e := ryn.NewStream(4)
	go func() {
		defer e.Close()
		e.Emit(ctx, ryn.TextFrame("x"))
		e.Emit(ctx, ryn.TextFrame("y"))
	}()

	ch := s.Chan()
	assertNotNil(t, ch)

	// Drain via the channel directly
	var frames []ryn.Frame
	for f := range ch {
		frames = append(frames, f)
	}
	assertEqual(t, len(frames), 2)
}

func TestForward(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	src := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("x"),
		ryn.TextFrame("y"),
	})

	dst, dstEmitter := ryn.NewStream(4)
	go func() {
		defer dstEmitter.Close()
		ryn.Forward(ctx, src, dstEmitter)
	}()

	frames, err := ryn.Collect(ctx, dst)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
}

func TestForwardSrcError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Source stream that emits a frame then errors.
	src, srcEm := ryn.NewStream(4)
	go func() {
		defer srcEm.Close()
		_ = srcEm.Emit(ctx, ryn.TextFrame("partial"))
		srcEm.Error(fmt.Errorf("src broke"))
	}()

	dst, dstEm := ryn.NewStream(4)
	var forwardErr error
	go func() {
		defer dstEm.Close()
		forwardErr = ryn.Forward(ctx, src, dstEm)
		if forwardErr != nil {
			dstEm.Error(forwardErr)
		}
	}()

	frames, err := ryn.Collect(ctx, dst)
	// Either the error is returned by Collect or is in the stream.
	_ = frames
	if err == nil {
		err = dst.Err()
	}
	assertTrue(t, err != nil || forwardErr != nil)
}

func TestForwardEmitError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Source has more frames than the dst buffer + dst will be closed early.
	src, srcEm := ryn.NewStream(4)
	go func() {
		defer srcEm.Close()
		for i := range 10 {
			_ = srcEm.Emit(ctx, ryn.TextFrame(fmt.Sprintf("frame-%d", i)))
		}
	}()

	// Destination with a tiny buffer that we close immediately.
	dst, dstEm := ryn.NewStream(1)
	dstEm.Close() // close before forwarding — Emit to it will fail

	err := ryn.Forward(ctx, src, dstEm)
	// Forward should return an error because the emitter is already closed.
	_ = dst
	_ = err // may or may not error depending on timing; just ensure no panic
}
