package runtime_test

import (
	"context"
	"fmt"
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

func TestRuntimeProviderError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, fmt.Errorf("provider unavailable")
	})

	rt := runtime.New(mock)
	_, err := rt.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	assertTrue(t, err != nil)
}

func TestRuntimeProviderErrorWithHook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var errorCalled bool
	var endCalled bool

	h := &testHook{
		onEnd: func(ctx context.Context, info hook.GenerateEndInfo) {
			endCalled = true
			assertTrue(t, info.Error != nil)
		},
	}
	_ = h

	fullH := &fullTestHook{
		onError: func(ctx context.Context, err error) {
			errorCalled = true
		},
		onEnd: func(ctx context.Context, info hook.GenerateEndInfo) {
			endCalled = true
		},
	}

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, fmt.Errorf("provider unavailable")
	})

	rt := runtime.New(mock).WithHook(fullH)
	_, err := rt.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	assertTrue(t, err != nil)
	assertTrue(t, errorCalled)
	assertTrue(t, endCalled)
}

func TestRuntimeStreamErrorWithHook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var errorCalled bool

	fullH := &fullTestHook{
		onError: func(ctx context.Context, err error) {
			errorCalled = true
		},
	}

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(4)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.TextFrame("partial"))
			e.Error(fmt.Errorf("stream error"))
		}()
		return s, nil
	})

	rt := runtime.New(mock).WithHook(fullH)
	stream, err := rt.Generate(ctx, &ryn.Request{
		Model:    "test",
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	assertNoError(t, err)

	for stream.Next(ctx) {
	}
	time.Sleep(20 * time.Millisecond) // allow goroutine to fire

	assertTrue(t, stream.Err() != nil)
	assertTrue(t, errorCalled)
}

func TestRuntimeHookOnFrameError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fullH := &fullTestHook{
		onFrame: func(ctx context.Context, f ryn.Frame) error {
			return fmt.Errorf("frame rejected")
		},
	}

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(4)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.TextFrame("hello"))
		}()
		return s, nil
	})

	rt := runtime.New(mock).WithHook(fullH)
	stream, err := rt.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	assertNoError(t, err)

	for stream.Next(ctx) {
	}
	assertTrue(t, stream.Err() != nil)
}

func TestRuntimeWithModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var capturedModel string
	h := &testHook{
		onStart: func(ctx context.Context, info hook.GenerateStartInfo) context.Context {
			capturedModel = info.Model
			return ctx
		},
	}

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice(nil), nil
	})

	rt := runtime.New(mock).WithHook(h)
	stream, err := rt.Generate(ctx, &ryn.Request{
		Model:    "gpt-4",
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	assertNoError(t, err)
	for stream.Next(ctx) {
	}
	time.Sleep(10 * time.Millisecond)
	assertEqual(t, capturedModel, "gpt-4")
}

func TestRuntimeDefaultModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var capturedModel string
	h := &testHook{
		onStart: func(ctx context.Context, info hook.GenerateStartInfo) context.Context {
			capturedModel = info.Model
			return ctx
		},
	}

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice(nil), nil
	})

	rt := runtime.New(mock).WithHook(h)
	_, _ = rt.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	assertEqual(t, capturedModel, "(default)")
}
