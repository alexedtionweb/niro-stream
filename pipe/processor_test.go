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

	in := niro.StreamFromSlice([]niro.Frame{
		niro.TextFrame("a"),
		niro.TextFrame("b"),
	})

	out, emitter := niro.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.PassThrough().Process(ctx, in, emitter)
	}()

	frames, err := niro.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
}

func TestFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := niro.StreamFromSlice([]niro.Frame{
		niro.TextFrame("keep"),
		niro.AudioFrame(nil, ""),
		niro.TextFrame("also keep"),
	})

	out, emitter := niro.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.Filter(func(f niro.Frame) bool { return f.Kind == niro.KindText }).Process(ctx, in, emitter)
	}()

	frames, err := niro.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
}

func TestMap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := niro.StreamFromSlice([]niro.Frame{niro.TextFrame("hello")})

	out, emitter := niro.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.Map(func(f niro.Frame) niro.Frame {
			if f.Kind == niro.KindText {
				f.Text = "[" + f.Text + "]"
			}
			return f
		}).Process(ctx, in, emitter)
	}()

	text, err := niro.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "[hello]")
}

func TestTap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := niro.StreamFromSlice([]niro.Frame{
		niro.TextFrame("a"),
		niro.TextFrame("b"),
	})

	var count int
	out, emitter := niro.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.Tap(func(f niro.Frame) { count++ }).Process(ctx, in, emitter)
	}()

	frames, err := niro.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
	assertEqual(t, count, 2)
}

func TestTextOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := niro.StreamFromSlice([]niro.Frame{
		niro.TextFrame("keep"),
		niro.ControlFrame(niro.SignalFlush),
		niro.ImageFrame(nil, ""),
	})

	out, emitter := niro.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.TextOnly().Process(ctx, in, emitter)
	}()

	frames, err := niro.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 1)
	assertEqual(t, frames[0].Text, "keep")
}

func TestAccumulate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := niro.StreamFromSlice([]niro.Frame{
		niro.TextFrame("H"),
		niro.TextFrame("e"),
		niro.TextFrame("l"),
		niro.ControlFrame(niro.SignalFlush),
		niro.TextFrame("l"),
		niro.TextFrame("o"),
	})

	out, emitter := niro.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.Accumulate().Process(ctx, in, emitter)
	}()

	frames, err := niro.Collect(ctx, out)
	assertNoError(t, err)
	// 1 control frame (passed through) + 1 accumulated text frame
	assertEqual(t, len(frames), 2)

	var text string
	for _, f := range frames {
		if f.Kind == niro.KindText {
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
	in := niro.StreamFromSlice([]niro.Frame{
		niro.ControlFrame(niro.SignalFlush),
		niro.ControlFrame(niro.SignalFlush),
	})

	out, emitter := niro.NewStream(4)
	go func() {
		defer emitter.Close()
		pipe.Accumulate().Process(ctx, in, emitter)
	}()

	frames, err := niro.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
}

func TestAccumulateStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Stream that emits an error — covers the "return in.Err()" path in Accumulate.
	in, inEm := niro.NewStream(4)
	go func() {
		defer inEm.Close()
		inEm.Error(fmt.Errorf("stream broke"))
	}()

	out, outEm := niro.NewStream(4)
	go func() {
		defer outEm.Close()
		if err := pipe.Accumulate().Process(ctx, in, outEm); err != nil {
			outEm.Error(err)
		}
	}()

	_, err := niro.Collect(ctx, out)
	assertErrorContains(t, err, "stream broke")
}

func TestFilterStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in, inEm := niro.NewStream(4)
	go func() {
		defer inEm.Close()
		inEm.Error(fmt.Errorf("filter stream err"))
	}()

	out, outEm := niro.NewStream(4)
	go func() {
		defer outEm.Close()
		if err := pipe.Filter(func(f niro.Frame) bool { return true }).Process(ctx, in, outEm); err != nil {
			outEm.Error(err)
		}
	}()

	_, err := niro.Collect(ctx, out)
	assertErrorContains(t, err, "filter stream err")
}

func TestMapStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in, inEm := niro.NewStream(4)
	go func() {
		defer inEm.Close()
		inEm.Error(fmt.Errorf("map stream err"))
	}()

	out, outEm := niro.NewStream(4)
	go func() {
		defer outEm.Close()
		if err := pipe.Map(func(f niro.Frame) niro.Frame { return f }).Process(ctx, in, outEm); err != nil {
			outEm.Error(err)
		}
	}()

	_, err := niro.Collect(ctx, out)
	assertErrorContains(t, err, "map stream err")
}

func TestTapStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in, inEm := niro.NewStream(4)
	go func() {
		defer inEm.Close()
		inEm.Error(fmt.Errorf("tap stream err"))
	}()

	out, outEm := niro.NewStream(4)
	go func() {
		defer outEm.Close()
		if err := pipe.Tap(func(f niro.Frame) {}).Process(ctx, in, outEm); err != nil {
			outEm.Error(err)
		}
	}()

	_, err := niro.Collect(ctx, out)
	assertErrorContains(t, err, "tap stream err")
}
