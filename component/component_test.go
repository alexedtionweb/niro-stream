package component_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"ryn.dev/ryn/component"
)

type testComponent struct {
	name string
	caps []component.Capability

	startErr error
	closeErr error

	started atomic.Int32
	closed  atomic.Int32
}

func (c *testComponent) Name() string { return c.name }

func (c *testComponent) Capabilities() []component.Capability {
	return c.caps
}

func (c *testComponent) Start(ctx context.Context) error {
	_ = ctx
	c.started.Add(1)
	return c.startErr
}

func (c *testComponent) Close() error {
	c.closed.Add(1)
	return c.closeErr
}

func TestComponentHostBasic(t *testing.T) {
	t.Parallel()

	h := component.NewHost()
	c := &testComponent{name: "mem", caps: []component.Capability{component.CapabilityAgentMemory}}
	err := h.Register(c)
	assertNoError(t, err)

	got, ok := h.Get("mem")
	assertTrue(t, ok)
	assertNotNil(t, got)

	names := h.Names()
	assertEqual(t, len(names), 1)

	byCap := h.ByCapability(component.CapabilityAgentMemory)
	assertEqual(t, len(byCap), 1)

	err = h.Register(c)
	assertErrorContains(t, err, "already registered")
}

func TestComponentHostLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := component.NewHost()
	c1 := &testComponent{name: "c1"}
	c2 := &testComponent{name: "c2", closeErr: fmt.Errorf("close fail")}
	assertNoError(t, h.Register(c1))
	assertNoError(t, h.Register(c2))

	assertNoError(t, h.StartAll(ctx))
	assertEqual(t, c1.started.Load() > 0, true)
	assertEqual(t, c2.started.Load() > 0, true)

	err := h.CloseAll()
	assertErrorContains(t, err, "close fail")
}

func TestComponentHostStartError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := component.NewHost()
	assertNoError(t, h.Register(&testComponent{name: "ok"}))
	assertNoError(t, h.Register(&testComponent{name: "bad", startErr: fmt.Errorf("boom")}))

	err := h.StartAll(ctx)
	assertErrorContains(t, err, "boom")
}

// --- test helpers ---

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

func assertErrorContains(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Errorf("expected error containing %q, got nil", substr)
		return
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("error %q does not contain %q", err.Error(), substr)
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
