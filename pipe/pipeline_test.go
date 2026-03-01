package pipe_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/pipe"
)

func TestPipelineEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := niro.StreamFromSlice([]niro.Frame{niro.TextFrame("x")})
	out := pipe.New().Run(ctx, in)

	frames, err := niro.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 1)
}

func TestPipelineSingleStage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := niro.StreamFromSlice([]niro.Frame{
		niro.TextFrame("a"),
		niro.TextFrame("b"),
	})

	p := pipe.New(
		pipe.Map(func(f niro.Frame) niro.Frame {
			f.Text = f.Text + "!"
			return f
		}),
	)

	out := p.Run(ctx, in)
	text, err := niro.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "a!b!")
}

func TestPipelineMultiStage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := niro.StreamFromSlice([]niro.Frame{
		niro.TextFrame("hello"),
		niro.AudioFrame(nil, ""),
		niro.TextFrame(" world"),
	})

	p := pipe.New(
		pipe.TextOnly(),
		pipe.Map(func(f niro.Frame) niro.Frame {
			f.Text = "[" + f.Text + "]"
			return f
		}),
	).WithBuffer(8)

	out := p.Run(ctx, in)
	text, err := niro.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "[hello][ world]")
}

func TestPipelineWithAccumulate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := niro.StreamFromSlice([]niro.Frame{
		niro.TextFrame("H"),
		niro.TextFrame("i"),
	})

	p := pipe.New(pipe.Accumulate())
	out := p.Run(ctx, in)

	text, err := niro.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "Hi")
}

func TestPipelineProcessorError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := niro.StreamFromSlice([]niro.Frame{niro.TextFrame("x")})

	errProc := pipe.ProcessorFunc(func(ctx context.Context, in *niro.Stream, out *niro.Emitter) error {
		for in.Next(ctx) {
			// consume but don't forward
		}
		return fmt.Errorf("processor failed")
	})

	p := pipe.New(errProc)
	out := p.Run(ctx, in)

	_, err := niro.Collect(ctx, out)
	assertErrorContains(t, err, "processor failed")
}
