package hook_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/hook"
)

type countingHook struct {
	hook.NoOpHook
	onFrame func()
}

func (h *countingHook) OnFrame(_ context.Context, _ ryn.Frame, _ time.Duration) error {
	if h.onFrame != nil {
		h.onFrame()
	}
	return nil
}

func TestNoOpHookSatisfiesInterface(t *testing.T) {
	t.Parallel()
	var h hook.Hook = hook.NoOpHook{}
	ctx := h.OnGenerateStart(context.Background(), hook.GenerateStartInfo{})
	assertNotNil(t, ctx)
	assertEqual(t, h.OnFrame(ctx, ryn.TextFrame("x"), 0), nil)
}

func TestNoOpHookAllMethods(t *testing.T) {
	t.Parallel()
	h := hook.NoOpHook{}
	ctx := context.Background()

	h.OnGenerateEnd(ctx, hook.GenerateEndInfo{})
	h.OnToolCall(ctx, ryn.ToolCall{})
	h.OnToolResult(ctx, ryn.ToolResult{}, 0)
	h.OnError(ctx, errors.New("err"))
	assertEqual(t, h.OnFrame(ctx, ryn.TextFrame("x"), 0), nil)
}

func TestHooksComposition(t *testing.T) {
	t.Parallel()

	var count1, count2 atomic.Int32

	h1 := &countingHook{onFrame: func() { count1.Add(1) }}
	h2 := &countingHook{onFrame: func() { count2.Add(1) }}

	combined := hook.Compose(h1, nil, h2)
	ctx := context.Background()

	combined.OnGenerateStart(ctx, hook.GenerateStartInfo{})
	combined.OnFrame(ctx, ryn.TextFrame("a"), 0)
	combined.OnFrame(ctx, ryn.TextFrame("b"), 0)

	assertEqual(t, int(count1.Load()), 2)
	assertEqual(t, int(count2.Load()), 2)
}

func TestHooksSingleNonNil(t *testing.T) {
	t.Parallel()
	h := &countingHook{}
	combined := hook.Compose(nil, h, nil)
	ctx := combined.OnGenerateStart(context.Background(), hook.GenerateStartInfo{})
	assertNotNil(t, ctx)
}

func TestHooksAllNil(t *testing.T) {
	t.Parallel()
	combined := hook.Compose(nil, nil)
	assertNil(t, combined)
}

func TestMultiHookGenerateEnd(t *testing.T) {
	t.Parallel()
	var endCalled1, endCalled2 atomic.Bool

	h1 := &fullHook{onEnd: func(_ context.Context, _ hook.GenerateEndInfo) { endCalled1.Store(true) }}
	h2 := &fullHook{onEnd: func(_ context.Context, _ hook.GenerateEndInfo) { endCalled2.Store(true) }}

	combined := hook.Compose(h1, h2)
	ctx := context.Background()
	combined.OnGenerateEnd(ctx, hook.GenerateEndInfo{Model: "gpt-4o", Duration: time.Second})

	assertTrue(t, endCalled1.Load())
	assertTrue(t, endCalled2.Load())
}

func TestMultiHookToolCallAndResult(t *testing.T) {
	t.Parallel()
	var toolCallCount, toolResultCount atomic.Int32

	h1 := &fullHook{
		onToolCall: func(_ context.Context, call ryn.ToolCall) {
			toolCallCount.Add(1)
		},
		onToolResult: func(_ context.Context, result ryn.ToolResult, elapsed time.Duration) {
			toolResultCount.Add(1)
		},
	}
	h2 := &fullHook{
		onToolCall: func(_ context.Context, call ryn.ToolCall) {
			toolCallCount.Add(1)
		},
		onToolResult: func(_ context.Context, result ryn.ToolResult, elapsed time.Duration) {
			toolResultCount.Add(1)
		},
	}

	combined := hook.Compose(h1, h2)
	ctx := context.Background()
	combined.OnToolCall(ctx, ryn.ToolCall{ID: "c1", Name: "weather"})
	combined.OnToolResult(ctx, ryn.ToolResult{CallID: "c1", Content: "sunny"}, 50*time.Millisecond)

	assertEqual(t, int(toolCallCount.Load()), 2)
	assertEqual(t, int(toolResultCount.Load()), 2)
}

func TestMultiHookOnError(t *testing.T) {
	t.Parallel()
	var errCount atomic.Int32

	h1 := &fullHook{onError: func(_ context.Context, err error) { errCount.Add(1) }}
	h2 := &fullHook{onError: func(_ context.Context, err error) { errCount.Add(1) }}

	combined := hook.Compose(h1, h2)
	combined.OnError(context.Background(), errors.New("test error"))

	assertEqual(t, int(errCount.Load()), 2)
}

func TestMultiHookOnFrameFirstErrorPropagates(t *testing.T) {
	t.Parallel()
	firstErr := errors.New("first error")

	// h1 returns an error, h2 should still be called but first error propagates
	var h2Called atomic.Bool
	h1 := &fullHook{onFrame: func(_ context.Context, _ ryn.Frame, _ time.Duration) error { return firstErr }}
	h2 := &fullHook{onFrame: func(_ context.Context, _ ryn.Frame, _ time.Duration) error {
		h2Called.Store(true)
		return nil
	}}

	combined := hook.Compose(h1, h2)
	err := combined.OnFrame(context.Background(), ryn.TextFrame("x"), 0)
	assertEqual(t, err, firstErr)
	assertTrue(t, h2Called.Load()) // h2 was still called
}

func TestHookGenerateStartInfo(t *testing.T) {
	t.Parallel()
	var capturedInfo hook.GenerateStartInfo

	h := &fullHook{
		onStart: func(ctx context.Context, info hook.GenerateStartInfo) context.Context {
			capturedInfo = info
			return ctx
		},
	}

	combined := hook.Compose(h)
	ctx := context.Background()
	combined.OnGenerateStart(ctx, hook.GenerateStartInfo{
		Provider:   "openai",
		Model:      "gpt-4o",
		Messages:   5,
		Tools:      2,
		RequestID:  "req_123",
		FunctionID: "fn_abc",
		Metadata:   map[string]string{"env": "prod"},
	})

	assertEqual(t, capturedInfo.Provider, "openai")
	assertEqual(t, capturedInfo.Model, "gpt-4o")
	assertEqual(t, capturedInfo.Messages, 5)
	assertEqual(t, capturedInfo.Tools, 2)
	assertEqual(t, capturedInfo.RequestID, "req_123")
}

// fullHook is a test hook that delegates all methods.
type fullHook struct {
	hook.NoOpHook
	onStart      func(ctx context.Context, info hook.GenerateStartInfo) context.Context
	onEnd        func(ctx context.Context, info hook.GenerateEndInfo)
	onFrame      func(ctx context.Context, f ryn.Frame, elapsed time.Duration) error
	onToolCall   func(ctx context.Context, call ryn.ToolCall)
	onToolResult func(ctx context.Context, result ryn.ToolResult, elapsed time.Duration)
	onError      func(ctx context.Context, err error)
}

func (h *fullHook) OnGenerateStart(ctx context.Context, info hook.GenerateStartInfo) context.Context {
	if h.onStart != nil {
		return h.onStart(ctx, info)
	}
	return ctx
}

func (h *fullHook) OnGenerateEnd(ctx context.Context, info hook.GenerateEndInfo) {
	if h.onEnd != nil {
		h.onEnd(ctx, info)
	}
}

func (h *fullHook) OnFrame(ctx context.Context, f ryn.Frame, elapsed time.Duration) error {
	if h.onFrame != nil {
		return h.onFrame(ctx, f, elapsed)
	}
	return nil
}

func (h *fullHook) OnToolCall(ctx context.Context, call ryn.ToolCall) {
	if h.onToolCall != nil {
		h.onToolCall(ctx, call)
	}
}

func (h *fullHook) OnToolResult(ctx context.Context, result ryn.ToolResult, elapsed time.Duration) {
	if h.onToolResult != nil {
		h.onToolResult(ctx, result, elapsed)
	}
}

func (h *fullHook) OnError(ctx context.Context, err error) {
	if h.onError != nil {
		h.onError(ctx, err)
	}
}

// --- test helpers ---

func assertEqual[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func assertNotNil(t *testing.T, v any) {
	t.Helper()
	if v == nil {
		t.Error("expected non-nil")
	}
}

func assertTrue(t *testing.T, v bool) {
	t.Helper()
	if !v {
		t.Error("expected true")
	}
}
func assertNil(t *testing.T, v any) {
	t.Helper()
	if v != nil {
		t.Errorf("expected nil, got %v", v)
	}
}
