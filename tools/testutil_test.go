package tools_test

import (
	"context"
	"strings"
	"testing"

	"ryn.dev/ryn"
	"ryn.dev/ryn/tools"
)

// toolHookMock implements tools.ToolRuntimeHook for testing.
type toolHookMock struct {
	validateCalls int
	startCalls    int
	endCalls      int
}

func (h *toolHookMock) OnToolValidate(ctx context.Context, info tools.ToolValidationInfo) {
	h.validateCalls++
}

func (h *toolHookMock) OnToolExecuteStart(ctx context.Context, call ryn.ToolCall) {
	h.startCalls++
}

func (h *toolHookMock) OnToolExecuteEnd(ctx context.Context, info tools.ToolExecutionInfo) {
	h.endCalls++
}

// nativeInputProviderMock supports the InputStreamingProvider interface.
type nativeInputProviderMock struct {
	called bool
}

func (m *nativeInputProviderMock) Generate(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
	return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("fallback")}), nil
}

func (m *nativeInputProviderMock) GenerateInputStream(ctx context.Context, req *ryn.Request, input *ryn.Stream) (*ryn.Stream, error) {
	m.called = true
	return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("native")}), nil
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
