package ryn_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// errTest is a sentinel error used across tests.
var errTest = fmt.Errorf("test error")

// --- Assert helpers ---

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
		return
	}
	// Handle typed nils (e.g. (*ryn.Error)(nil) wrapped in any)
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr && rv.IsNil() {
		t.Error("expected non-nil")
	}
}

func assertNil(t *testing.T, v any) {
	t.Helper()
	if v == nil {
		return
	}
	// Handle typed nils (e.g. (*ryn.Error)(nil) wrapped in any)
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr && rv.IsNil() {
		return
	}
	t.Errorf("expected nil, got %v", v)
}

func assertTrue(t *testing.T, v bool) {
	t.Helper()
	if !v {
		t.Error("expected true")
	}
}
