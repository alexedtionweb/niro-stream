package hook_test

import (
	"context"
	"sync/atomic"
	"testing"

	"ryn.dev/ryn"
	"ryn.dev/ryn/hook"
)

type countingHook struct {
	hook.NoOpHook
	onFrame func()
}

func (h *countingHook) OnFrame(_ context.Context, _ ryn.Frame) error {
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
	assertEqual(t, h.OnFrame(ctx, ryn.TextFrame("x")), nil)
}

func TestHooksComposition(t *testing.T) {
	t.Parallel()

	var count1, count2 atomic.Int32

	h1 := &countingHook{onFrame: func() { count1.Add(1) }}
	h2 := &countingHook{onFrame: func() { count2.Add(1) }}

	combined := hook.Compose(h1, nil, h2)
	ctx := context.Background()

	combined.OnGenerateStart(ctx, hook.GenerateStartInfo{})
	combined.OnFrame(ctx, ryn.TextFrame("a"))
	combined.OnFrame(ctx, ryn.TextFrame("b"))

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

func assertNil(t *testing.T, v any) {
	t.Helper()
	if v != nil {
		t.Errorf("expected nil, got %v", v)
	}
}
