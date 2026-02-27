package runtime_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"ryn.dev/ryn"
	"ryn.dev/ryn/hook"
	"ryn.dev/ryn/pipe"
	"ryn.dev/ryn/runtime"
)

func TestRuntimeBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(4)
		go func() {
			defer e.Close()
			e.Emit(ctx, ryn.TextFrame("hello"))
			e.SetResponse(&ryn.ResponseMeta{Model: "test", FinishReason: "stop"})
		}()
		return s, nil
	})

	rt := runtime.New(mock)
	stream, err := rt.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	assertNoError(t, err)

	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "hello")
}

func TestRuntimeWithPipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(4)
		go func() {
			defer e.Close()
			e.Emit(ctx, ryn.TextFrame("H"))
			e.Emit(ctx, ryn.TextFrame("i"))
		}()
		return s, nil
	})

	rt := runtime.New(mock).WithPipeline(pipe.New(pipe.Accumulate()))
	stream, err := rt.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("test")},
	})
	assertNoError(t, err)

	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "Hi")
}

func TestRuntimeWithHook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var (
		startCalled atomic.Bool
		endCalled   atomic.Bool
		frameCnt    atomic.Int32
	)

	h := &testHook{
		onStart: func(ctx context.Context, info hook.GenerateStartInfo) context.Context {
			startCalled.Store(true)
			assertEqual(t, info.Model, "test-model")
			return ctx
		},
		onEnd: func(ctx context.Context, info hook.GenerateEndInfo) {
			endCalled.Store(true)
			assertEqual(t, info.Model, "test-model")
		},
		onFrame: func(ctx context.Context, f ryn.Frame) error {
			frameCnt.Add(1)
			return nil
		},
	}

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(4)
		go func() {
			defer e.Close()
			e.Emit(ctx, ryn.TextFrame("a"))
			e.Emit(ctx, ryn.TextFrame("b"))
			e.SetResponse(&ryn.ResponseMeta{Model: "test-model"})
		}()
		return s, nil
	})

	rt := runtime.New(mock).WithHook(h)
	stream, err := rt.Generate(ctx, &ryn.Request{
		Model:    "test-model",
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	assertNoError(t, err)

	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "ab")

	// Give the goroutine a moment to fire OnGenerateEnd
	time.Sleep(20 * time.Millisecond)

	assertTrue(t, startCalled.Load())
	assertTrue(t, endCalled.Load())
	assertEqual(t, int(frameCnt.Load()), 2)
}
