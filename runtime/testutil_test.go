package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/hook"
)

type testHook struct {
	hook.NoOpHook
	onStart func(context.Context, hook.GenerateStartInfo) context.Context
	onEnd   func(context.Context, hook.GenerateEndInfo)
	onFrame func(context.Context, niro.Frame, time.Duration) error
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

func (h *testHook) OnFrame(ctx context.Context, f niro.Frame, elapsed time.Duration) error {
	if h.onFrame != nil {
		return h.onFrame(ctx, f, elapsed)
	}
	return nil
}

type fullTestHook struct {
	hook.NoOpHook
	onStart func(context.Context, hook.GenerateStartInfo) context.Context
	onEnd   func(context.Context, hook.GenerateEndInfo)
	onFrame func(context.Context, niro.Frame, time.Duration) error
	onError func(context.Context, error)
}

func (h *fullTestHook) OnGenerateStart(ctx context.Context, info hook.GenerateStartInfo) context.Context {
	if h.onStart != nil {
		return h.onStart(ctx, info)
	}
	return ctx
}

func (h *fullTestHook) OnGenerateEnd(ctx context.Context, info hook.GenerateEndInfo) {
	if h.onEnd != nil {
		h.onEnd(ctx, info)
	}
}

func (h *fullTestHook) OnFrame(ctx context.Context, f niro.Frame, elapsed time.Duration) error {
	if h.onFrame != nil {
		return h.onFrame(ctx, f, elapsed)
	}
	return nil
}

func (h *fullTestHook) OnError(ctx context.Context, err error) {
	if h.onError != nil {
		h.onError(ctx, err)
	}
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
