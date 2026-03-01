package pipe_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/pipe"
)

func TestPassThrough(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("a"),
		ryn.TextFrame("b"),
	})

	out, emitter := ryn.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.PassThrough().Process(ctx, in, emitter)
	}()

	frames, err := ryn.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
}

func TestFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("keep"),
		ryn.AudioFrame(nil, ""),
		ryn.TextFrame("also keep"),
	})

	out, emitter := ryn.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.Filter(func(f ryn.Frame) bool { return f.Kind == ryn.KindText }).Process(ctx, in, emitter)
	}()

	frames, err := ryn.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
}

func TestMap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("hello")})

	out, emitter := ryn.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.Map(func(f ryn.Frame) ryn.Frame {
			if f.Kind == ryn.KindText {
				f.Text = "[" + f.Text + "]"
			}
			return f
		}).Process(ctx, in, emitter)
	}()

	text, err := ryn.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "[hello]")
}

func TestTap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("a"),
		ryn.TextFrame("b"),
	})

	var count int
	out, emitter := ryn.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.Tap(func(f ryn.Frame) { count++ }).Process(ctx, in, emitter)
	}()

	frames, err := ryn.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
	assertEqual(t, count, 2)
}

func TestTextOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("keep"),
		ryn.ControlFrame(ryn.SignalFlush),
		ryn.ImageFrame(nil, ""),
	})

	out, emitter := ryn.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.TextOnly().Process(ctx, in, emitter)
	}()

	frames, err := ryn.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 1)
	assertEqual(t, frames[0].Text, "keep")
}

func TestAccumulate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("H"),
		ryn.TextFrame("e"),
		ryn.TextFrame("l"),
		ryn.ControlFrame(ryn.SignalFlush),
		ryn.TextFrame("l"),
		ryn.TextFrame("o"),
	})

	out, emitter := ryn.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.Accumulate().Process(ctx, in, emitter)
	}()

	frames, err := ryn.Collect(ctx, out)
	assertNoError(t, err)
	// 1 control frame (passed through) + 1 accumulated text frame
	assertEqual(t, len(frames), 2)

	var text string
	for _, f := range frames {
		if f.Kind == ryn.KindText {
			text = f.Text
		}
	}
	assertEqual(t, text, "Hello")
}

func TestAccumulateNoText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Only non-text frames: they pass through, and no text is accumulated,
	// so the final "if len(buf) > 0" is false → covers the "return nil" path.
	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.ControlFrame(ryn.SignalFlush),
		ryn.ControlFrame(ryn.SignalFlush),
	})

	out, emitter := ryn.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.Accumulate().Process(ctx, in, emitter)
	}()

	frames, err := ryn.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
}

func TestAccumulateStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Stream that emits an error — covers the "return in.Err()" path in Accumulate.
	in, inEm := ryn.NewStream(4)
	go func() {
		defer inEm.Close()
		inEm.Error(fmt.Errorf("stream broke"))
	}()

	out, outEm := ryn.NewStream(4)
	go func() {
		defer outEm.Close()
		if err := pipe.Accumulate().Process(ctx, in, outEm); err != nil {
			outEm.Error(err)
		}
	}()

	_, err := ryn.Collect(ctx, out)
	assertErrorContains(t, err, "stream broke")
}

func TestFilterStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in, inEm := ryn.NewStream(4)
	go func() {
		defer inEm.Close()
		inEm.Error(fmt.Errorf("filter stream err"))
	}()

	out, outEm := ryn.NewStream(4)
	go func() {
		defer outEm.Close()
		if err := pipe.Filter(func(f ryn.Frame) bool { return true }).Process(ctx, in, outEm); err != nil {
			outEm.Error(err)
		}
	}()

	_, err := ryn.Collect(ctx, out)
	assertErrorContains(t, err, "filter stream err")
}

func TestMapStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in, inEm := ryn.NewStream(4)
	go func() {
		defer inEm.Close()
		inEm.Error(fmt.Errorf("map stream err"))
	}()

	out, outEm := ryn.NewStream(4)
	go func() {
		defer outEm.Close()
		if err := pipe.Map(func(f ryn.Frame) ryn.Frame { return f }).Process(ctx, in, outEm); err != nil {
			outEm.Error(err)
		}
	}()

	_, err := ryn.Collect(ctx, out)
	assertErrorContains(t, err, "map stream err")
}

func TestTapStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in, inEm := ryn.NewStream(4)
	go func() {
		defer inEm.Close()
		inEm.Error(fmt.Errorf("tap stream err"))
	}()

	out, outEm := ryn.NewStream(4)
	go func() {
		defer outEm.Close()
		if err := pipe.Tap(func(f ryn.Frame) {}).Process(ctx, in, outEm); err != nil {
			outEm.Error(err)
		}
	}()

	_, err := ryn.Collect(ctx, out)
	assertErrorContains(t, err, "tap stream err")
}
