package ryn_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"ryn.dev/ryn"
)

func TestPipelineSingleProcessor(t *testing.T) {
	ctx := context.Background()
	input := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("hello"),
		ryn.TextFrame(" "),
		ryn.TextFrame("world"),
	})

	upper := ryn.Map(func(f ryn.Frame) ryn.Frame {
		if f.Kind == ryn.KindText {
			f.Text = strings.ToUpper(f.Text)
		}
		return f
	})

	out := ryn.Pipe(upper).Run(ctx, input)

	got, err := ryn.CollectText(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if got != "HELLO WORLD" {
		t.Errorf("got %q, want %q", got, "HELLO WORLD")
	}
}

func TestPipelineChain(t *testing.T) {
	ctx := context.Background()
	input := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("hello"),
		ryn.ControlFrame(ryn.SignalFlush),
		ryn.TextFrame("world"),
	})

	p := ryn.Pipe(
		// Stage 1: filter to text only
		ryn.TextOnly(),
		// Stage 2: uppercase
		ryn.Map(func(f ryn.Frame) ryn.Frame {
			f.Text = strings.ToUpper(f.Text)
			return f
		}),
	)

	out := p.Run(ctx, input)

	got, err := ryn.Collect(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2", len(got))
	}
	if got[0].Text != "HELLO" || got[1].Text != "WORLD" {
		t.Errorf("got %q %q, want HELLO WORLD", got[0].Text, got[1].Text)
	}
}

func TestPipelineEmpty(t *testing.T) {
	ctx := context.Background()
	input := ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("pass")})

	// Empty pipeline should return input unchanged
	out := ryn.Pipe().Run(ctx, input)

	got, err := ryn.CollectText(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if got != "pass" {
		t.Errorf("got %q, want %q", got, "pass")
	}
}

func TestPipelineTap(t *testing.T) {
	ctx := context.Background()
	input := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("a"),
		ryn.TextFrame("b"),
		ryn.TextFrame("c"),
	})

	var tapped []string
	p := ryn.Pipe(
		ryn.Tap(func(f ryn.Frame) {
			tapped = append(tapped, f.Text)
		}),
	)
	out := p.Run(ctx, input)

	got, err := ryn.CollectText(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if got != "abc" {
		t.Errorf("output: got %q, want %q", got, "abc")
	}
	if len(tapped) != 3 {
		t.Errorf("tapped %d frames, want 3", len(tapped))
	}
}

func TestPipelineCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Create a stream that will block forever
	input, _ := ryn.NewStream(0)

	p := ryn.Pipe(ryn.PassThrough())
	out := p.Run(ctx, input)

	// Cancel should unblock everything
	cancel()

	if out.Next(ctx) {
		t.Error("expected Next to return false after cancel")
	}
}

func TestPipelineErrorPropagation(t *testing.T) {
	ctx := context.Background()
	input := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("a"),
		ryn.TextFrame("b"),
	})

	failing := ryn.ProcessorFunc(func(ctx context.Context, in *ryn.Stream, out *ryn.Emitter) error {
		// Read one frame, then fail
		if in.Next(ctx) {
			out.Emit(ctx, in.Frame())
		}
		return fmt.Errorf("intentional failure")
	})

	out := ryn.Pipe(failing).Run(ctx, input)

	// May get one frame before error
	for out.Next(ctx) {
		// consume
	}

	if out.Err() == nil {
		t.Error("expected error from pipeline")
	}
}

func TestPipelineWithBuffer(t *testing.T) {
	ctx := context.Background()
	input := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("x"),
	})

	p := ryn.Pipe(ryn.PassThrough()).WithBuffer(64)
	out := p.Run(ctx, input)

	got, err := ryn.CollectText(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if got != "x" {
		t.Errorf("got %q, want %q", got, "x")
	}
}

func TestProviderFunc(t *testing.T) {
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			for _, m := range req.Messages {
				for _, p := range m.Parts {
					if p.Kind == ryn.KindText {
						e.Emit(ctx, ryn.TextFrame("echo: "+p.Text))
					}
				}
			}
		}()
		return s, nil
	})

	stream, err := mock.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.Text(ryn.RoleUser, "hello")},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := ryn.CollectText(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo: hello" {
		t.Errorf("got %q, want %q", got, "echo: hello")
	}
}

func TestRuntimeWithPipeline(t *testing.T) {
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{
			ryn.TextFrame("hello"),
			ryn.TextFrame(" world"),
		}), nil
	})

	rt := ryn.NewRuntime(mock).WithPipeline(
		ryn.Pipe(ryn.Map(func(f ryn.Frame) ryn.Frame {
			if f.Kind == ryn.KindText {
				f.Text = strings.ToUpper(f.Text)
			}
			return f
		})),
	)

	stream, err := rt.Generate(ctx, &ryn.Request{})
	if err != nil {
		t.Fatal(err)
	}

	got, err := ryn.CollectText(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	if got != "HELLO WORLD" {
		t.Errorf("got %q, want %q", got, "HELLO WORLD")
	}
}
