package middleware_test

import (
	"context"
	"strings"
	"testing"

	"github.com/alexedtionweb/niro-stream"
)

// retryHintProviderMock is a provider that can declare it handles retries internally.
type retryHintProviderMock struct {
	handlesRetries bool
	fn             func(ctx context.Context, req *niro.Request) (*niro.Stream, error)
}

func (m *retryHintProviderMock) Generate(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
	return m.fn(ctx, req)
}

func (m *retryHintProviderMock) ProviderHandlesRetries() bool {
	return m.handlesRetries
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

func assertNil(t *testing.T, v any) {
	t.Helper()
	if v != nil {
		t.Errorf("expected nil, got %v", v)
	}
}

func assertTrue(t *testing.T, v bool) {
	t.Helper()
	if !v {
		t.Error("expected true")
	}
}

// TestAssertHelpers ensures assertNil/assertNotNil are exercised (used by other tests in this package).
func TestAssertHelpers(t *testing.T) {
	assertNil(t, nil)
	assertNotNil(t, &retryHintProviderMock{})
}
