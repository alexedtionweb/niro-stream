package runtime_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/hook"
	"github.com/alexedtionweb/niro-stream/pipe"
	"github.com/alexedtionweb/niro-stream/runtime"
)

func TestRuntimeBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s, e := niro.NewStream(4)
		go func() {
			defer e.Close()
			e.Emit(ctx, niro.TextFrame("hello"))
			e.SetResponse(&niro.ResponseMeta{Model: "test", FinishReason: "stop"})
		}()
		return s, nil
	})

	rt := runtime.New(mock)
	stream, err := rt.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	assertNoError(t, err)

	text, err := niro.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "hello")
}

func TestRuntimeWithPipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s, e := niro.NewStream(4)
		go func() {
			defer e.Close()
			e.Emit(ctx, niro.TextFrame("H"))
			e.Emit(ctx, niro.TextFrame("i"))
		}()
		return s, nil
	})

	rt := runtime.New(mock).WithPipeline(pipe.New(pipe.Accumulate()))
	stream, err := rt.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("test")},
	})
	assertNoError(t, err)

	text, err := niro.CollectText(ctx, stream)
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
		onFrame: func(ctx context.Context, f niro.Frame, elapsed time.Duration) error {
			frameCnt.Add(1)
			return nil
		},
	}

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s, e := niro.NewStream(4)
		go func() {
			defer e.Close()
			e.Emit(ctx, niro.TextFrame("a"))
			e.Emit(ctx, niro.TextFrame("b"))
			e.SetResponse(&niro.ResponseMeta{Model: "test-model"})
		}()
		return s, nil
	})

	rt := runtime.New(mock).WithHook(h)
	stream, err := rt.Generate(ctx, &niro.Request{
		Model:    "test-model",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	assertNoError(t, err)

	text, err := niro.CollectText(ctx, stream)
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

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return nil, fmt.Errorf("provider unavailable")
	})

	rt := runtime.New(mock)
	_, err := rt.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
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

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return nil, fmt.Errorf("provider unavailable")
	})

	rt := runtime.New(mock).WithHook(fullH)
	_, err := rt.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	assertTrue(t, err != nil)
	assertTrue(t, errorCalled)
	assertTrue(t, endCalled)
}

func TestRuntimeStreamErrorWithHook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var errorCalled atomic.Bool

	fullH := &fullTestHook{
		onError: func(ctx context.Context, err error) {
			errorCalled.Store(true)
		},
	}

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s, e := niro.NewStream(4)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, niro.TextFrame("partial"))
			e.Error(fmt.Errorf("stream error"))
		}()
		return s, nil
	})

	rt := runtime.New(mock).WithHook(fullH)
	stream, err := rt.Generate(ctx, &niro.Request{
		Model:    "test",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	assertNoError(t, err)

	for stream.Next(ctx) {
	}
	time.Sleep(20 * time.Millisecond) // allow goroutine to fire

	assertTrue(t, stream.Err() != nil)
	assertTrue(t, errorCalled.Load())
}

func TestRuntimeHookOnFrameError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fullH := &fullTestHook{
		onFrame: func(ctx context.Context, f niro.Frame, elapsed time.Duration) error {
			return fmt.Errorf("frame rejected")
		},
	}

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s, e := niro.NewStream(4)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, niro.TextFrame("hello"))
		}()
		return s, nil
	})

	rt := runtime.New(mock).WithHook(fullH)
	stream, err := rt.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
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

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice(nil), nil
	})

	rt := runtime.New(mock).WithHook(h)
	stream, err := rt.Generate(ctx, &niro.Request{
		Model:    "gpt-4",
		Messages: []niro.Message{niro.UserText("hi")},
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

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice(nil), nil
	})

	rt := runtime.New(mock).WithHook(h)
	_, _ = rt.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	assertEqual(t, capturedModel, "(default)")
}
