package pipe_test

import (
	"context"
	"testing"

	"ryn.dev/ryn"
	"ryn.dev/ryn/pipe"
)

func TestPipelineEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("x")})
	out := pipe.New().Run(ctx, in)

	frames, err := ryn.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 1)
}

func TestPipelineSingleStage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("a"),
		ryn.TextFrame("b"),
	})

	p := pipe.New(
		pipe.Map(func(f ryn.Frame) ryn.Frame {
			f.Text = f.Text + "!"
			return f
		}),
	)

	out := p.Run(ctx, in)
	text, err := ryn.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "a!b!")
}

func TestPipelineMultiStage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("hello"),
		ryn.AudioFrame(nil, ""),
		ryn.TextFrame(" world"),
	})

	p := pipe.New(
		pipe.TextOnly(),
		pipe.Map(func(f ryn.Frame) ryn.Frame {
			f.Text = "[" + f.Text + "]"
			return f
		}),
	).WithBuffer(8)

	out := p.Run(ctx, in)
	text, err := ryn.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "[hello][ world]")
}

func TestPipelineWithAccumulate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("H"),
		ryn.TextFrame("i"),
	})

	p := pipe.New(pipe.Accumulate())
	out := p.Run(ctx, in)

	text, err := ryn.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "Hi")
}
