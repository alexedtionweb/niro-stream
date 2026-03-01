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

func TestRuntimeWithHook_UsesResponseModelOnEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var endModel string
	h := &testHook{
		onEnd: func(ctx context.Context, info hook.GenerateEndInfo) {
			endModel = info.Model
		},
	}

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s, e := niro.NewStream(4)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, niro.TextFrame("ok"))
			e.SetResponse(&niro.ResponseMeta{Model: "gemini-2.5-pro-preview", FinishReason: "stop"})
		}()
		return s, nil
	})

	rt := runtime.New(mock).WithHook(h)
	stream, err := rt.Generate(ctx, &niro.Request{
		Model:    "gemini",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	assertNoError(t, err)

	_, err = niro.CollectText(ctx, stream)
	assertNoError(t, err)

	time.Sleep(20 * time.Millisecond)
	assertEqual(t, endModel, "gemini-2.5-pro-preview")
}

func TestRuntimeWithHook_DoesNotDoubleCountUsageFromResponse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var endUsage niro.Usage
	h := &testHook{
		onEnd: func(ctx context.Context, info hook.GenerateEndInfo) {
			endUsage = info.Usage
		},
	}

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s, e := niro.NewStream(4)
		go func() {
			defer e.Close()
			u := niro.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
			_ = e.Emit(ctx, niro.UsageFrame(&u))
			e.SetResponse(&niro.ResponseMeta{
				Model: "m",
				Usage: niro.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			})
		}()
		return s, nil
	})

	rt := runtime.New(mock).WithHook(h)
	stream, err := rt.Generate(ctx, &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	assertNoError(t, err)

	for stream.Next(ctx) {
	}
	assertNoError(t, stream.Err())

	time.Sleep(20 * time.Millisecond)
	assertEqual(t, endUsage.InputTokens, 10)
	assertEqual(t, endUsage.OutputTokens, 5)
	assertEqual(t, endUsage.TotalTokens, 15)
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

func TestRuntimeCacheRequireFailsWhenUnsupported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice(nil), nil
	})

	rt := runtime.New(mock)
	_, err := rt.Generate(ctx, &niro.Request{
		Client:   "tenant-a",
		Messages: []niro.Message{niro.UserText("hi")},
		Options: niro.Options{
			Cache: &niro.CacheOptions{
				Mode: niro.CacheRequire,
				Key:  "k",
			},
		},
	})
	assertTrue(t, err != nil)
}

func TestRuntimeCacheHintAttachedOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var gotHint niro.CacheHint
	var gotHintOK bool
	calls := 0

	norm := niro.PrefixNormalizerFunc(func(req *niro.Request) ([]byte, error) {
		calls++
		return []byte("same-prefix"), nil
	})

	mock := cacheCapProvider{ProviderFunc: niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		gotHint, gotHintOK = niro.GetCacheHint(ctx)
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
	})}

	rt := runtime.New(mock).WithPrefixNormalizer(norm)
	stream, err := rt.Generate(ctx, &niro.Request{
		Client:   "tenant-a",
		Messages: []niro.Message{niro.UserText("hi")},
		Options: niro.Options{
			Cache: &niro.CacheOptions{Mode: niro.CacheRequire},
		},
	})
	assertNoError(t, err)
	_, err = niro.CollectText(ctx, stream)
	assertNoError(t, err)
	assertTrue(t, gotHintOK)
	assertEqual(t, calls, 1)
	assertTrue(t, gotHint.Key != "")
	assertTrue(t, gotHint.PrefixHash != "")
}

func TestRuntimeCacheBypassDoesNotAttachHint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var gotHintOK bool
	mock := cacheCapProvider{ProviderFunc: niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		_, gotHintOK = niro.GetCacheHint(ctx)
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
	})}

	rt := runtime.New(mock)
	stream, err := rt.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Options: niro.Options{
			Cache: &niro.CacheOptions{Mode: niro.CacheBypass},
		},
	})
	assertNoError(t, err)
	_, err = niro.CollectText(ctx, stream)
	assertNoError(t, err)
	assertTrue(t, !gotHintOK)
}

func TestRuntimeCacheExplicitKeySkipsNormalizer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var calls int
	norm := niro.PrefixNormalizerFunc(func(req *niro.Request) ([]byte, error) {
		calls++
		return []byte("unused"), nil
	})

	rt := runtime.New(cacheCapProvider{ProviderFunc: niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
	})}).WithPrefixNormalizer(norm)

	stream, err := rt.Generate(ctx, &niro.Request{
		Client:   "tenant-a",
		Messages: []niro.Message{niro.UserText("hi")},
		Options: niro.Options{
			Cache: &niro.CacheOptions{
				Mode: niro.CachePrefer,
				Key:  "my-key",
			},
		},
	})
	assertNoError(t, err)
	_, err = niro.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, calls, 0)
}

type cacheCapProvider struct {
	niro.ProviderFunc
}

func (p cacheCapProvider) Generate(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
	return p.ProviderFunc(ctx, req)
}

func (cacheCapProvider) CacheCaps() niro.CacheCapabilities {
	return niro.CacheCapabilities{
		SupportsPrefix:       true,
		SupportsExplicitKeys: true,
		SupportsTTL:          true,
		SupportsBypass:       true,
	}
}
