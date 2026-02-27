package runtime_test

import (
	"context"
	"testing"

	"ryn.dev/ryn"
	"ryn.dev/ryn/hook"
)

type testHook struct {
	hook.NoOpHook
	onStart func(context.Context, hook.GenerateStartInfo) context.Context
	onEnd   func(context.Context, hook.GenerateEndInfo)
	onFrame func(context.Context, ryn.Frame) error
}

func (h *testHook) OnGenerateStart(ctx context.Context, info hook.GenerateStartInfo) context.Context {
	if h.onStart != nil {
		return h.onStart(ctx, info)
	}
	return ctx
}

func (h *testHook) OnGenerateEnd(ctx context.Context, info hook.GenerateEndInfo) {
	if h.onEnd != nil {
		h.onEnd(ctx, info)
	}
}

func (h *testHook) OnFrame(ctx context.Context, f ryn.Frame) error {
	if h.onFrame != nil {
		return h.onFrame(ctx, f)
	}
	return nil
}

func assertEqual[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func assertTrue(t *testing.T, v bool) {
	t.Helper()
	if !v {
		t.Error("expected true")
	}
}
