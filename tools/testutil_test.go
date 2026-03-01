package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/tools"
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

func (h *toolHookMock) OnToolExecuteStart(ctx context.Context, call niro.ToolCall) {
	h.startCalls++
}

func (h *toolHookMock) OnToolExecuteEnd(ctx context.Context, info tools.ToolExecutionInfo) {
	h.endCalls++
}

// nativeInputProviderMock supports the InputStreamingProvider interface.
type nativeInputProviderMock struct {
	called bool
}

func (m *nativeInputProviderMock) Generate(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
	return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("fallback")}), nil
}

func (m *nativeInputProviderMock) GenerateInputStream(ctx context.Context, req *niro.Request, input *niro.Stream) (*niro.Stream, error) {
	m.called = true
	return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("native")}), nil
}

// testSchemaValidator implements tools.ToolSchemaValidator for tests.
type testSchemaValidator struct {
	validateFn func(schema, args json.RawMessage) error
}

func (v *testSchemaValidator) Validate(schema, args json.RawMessage) error {
	if v.validateFn != nil {
		return v.validateFn(schema, args)
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
