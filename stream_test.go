package ryn_test

import (
	"context"
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
