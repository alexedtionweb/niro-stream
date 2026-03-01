package ryn_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

// ── DiscardHandler (slog.Handler) ────────────────────────────────────────────
// These tests verify the slog.Handler implementation kept for slog users.

func TestDiscardHandlerEnabled(t *testing.T) {
	h := ryn.DiscardHandler{}
	ctx := context.Background()
	for _, lvl := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		if h.Enabled(ctx, lvl) {
			t.Errorf("Enabled(%v) = true; want false", lvl)
		}
	}
}

func TestDiscardHandlerHandle(t *testing.T) {
	h := ryn.DiscardHandler{}
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
}

func TestDiscardHandlerWithAttrs(t *testing.T) {
	h := ryn.DiscardHandler{}
	got := h.WithAttrs([]slog.Attr{slog.String("k", "v")})
	if got != h {
		t.Error("WithAttrs should return the same DiscardHandler")
	}
}

func TestDiscardHandlerWithGroup(t *testing.T) {
	h := ryn.DiscardHandler{}
	got := h.WithGroup("grp")
	if got != h {
		t.Error("WithGroup should return the same DiscardHandler")
	}
}

// ── Discard() ────────────────────────────────────────────────────────────────

func TestDiscardNeverEnabled(t *testing.T) {
	d := ryn.Discard()
	if d == nil {
		t.Fatal("Discard() must not be nil")
	}
	ctx := context.Background()
	for _, level := range []ryn.Level{ryn.LevelDebug, ryn.LevelInfo, ryn.LevelWarn, ryn.LevelError} {
		if d.Enabled(ctx, level) {
			t.Errorf("Discard.Enabled(%v) = true; want false", level)
		}
	}
}

func TestDiscardLogNoPanic(t *testing.T) {
	// Log must not panic even when called directly (e.g. by a custom caller
	// that skips the Enabled check).
	ryn.Discard().Log(context.Background(), ryn.LevelError, "msg", "k", "v")
}

func TestDiscardSingleton(t *testing.T) {
	if ryn.Discard() != ryn.Discard() {
		t.Error("Discard() must return the same singleton on every call")
	}
}

// ── NewSlogAdapter ────────────────────────────────────────────────────────────

func TestNewSlogAdapterEnabled(t *testing.T) {
	var buf bytes.Buffer
	sl := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	l := ryn.NewSlogAdapter(sl)

	ctx := context.Background()
	if !l.Enabled(ctx, ryn.LevelDebug) {
		t.Error("adapter should be enabled at Debug when handler is at Debug")
	}
	if !l.Enabled(ctx, ryn.LevelError) {
		t.Error("adapter should be enabled at Error")
	}
}

func TestNewSlogAdapterEmits(t *testing.T) {
	var buf bytes.Buffer
	sl := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	l := ryn.NewSlogAdapter(sl)

	l.Log(context.Background(), ryn.LevelWarn, "hello-adapter", "k", "v")
	got := buf.String()
	if !strings.Contains(got, "hello-adapter") {
		t.Errorf("expected 'hello-adapter' in output, got: %s", got)
	}
	if !strings.Contains(got, "k=v") {
		t.Errorf("expected key-value 'k=v' in output, got: %s", got)
	}
}

func TestNewSlogAdapterNilReturnsDiscard(t *testing.T) {
	if ryn.NewSlogAdapter(nil) != ryn.Discard() {
		t.Error("NewSlogAdapter(nil) must return Discard()")
	}
}

// ── GetLogger / SetLogger / ResetLogger ──────────────────────────────────────

func TestSetLoggerReplaces(t *testing.T) {
	orig := ryn.GetLogger()
	t.Cleanup(func() { ryn.SetLogger(orig) })

	custom := ryn.Discard()
	ryn.SetLogger(custom)
	if ryn.GetLogger() != custom {
		t.Error("GetLogger() did not return the logger installed by SetLogger")
	}
}

func TestSetLoggerNilInstallsDiscard(t *testing.T) {
	orig := ryn.GetLogger()
	t.Cleanup(func() { ryn.SetLogger(orig) })

	ryn.SetLogger(nil)
	l := ryn.GetLogger()
	if l == nil {
		t.Fatal("GetLogger() must never return nil")
	}
	if l.Enabled(context.Background(), ryn.LevelError) {
		t.Error("SetLogger(nil) must install a discard logger (Enabled must be false)")
	}
}

func TestSetLoggerConcurrent(t *testing.T) {
	orig := ryn.GetLogger()
	t.Cleanup(func() { ryn.SetLogger(orig) })

	l1 := ryn.NewSlogAdapter(slog.New(ryn.DiscardHandler{}))
	l2 := ryn.Discard()

	var wg sync.WaitGroup
	for range 64 {
		wg.Add(2)
		go func() { defer wg.Done(); ryn.SetLogger(l1) }()
		go func() { defer wg.Done(); _ = ryn.GetLogger() }()
	}
	wg.Wait()

	ryn.SetLogger(l2)
	if ryn.GetLogger() != l2 {
		t.Error("GetLogger() should return l2 after the final SetLogger call")
	}
}

func TestGetLoggerDelegatesLiveToSlogDefault(t *testing.T) {
	ryn.ResetLogger()
	t.Cleanup(ryn.ResetLogger)

	// After ResetLogger, GetLogger must behave identically to slog.Default()
	// at call time — not a snapshot captured at init.
	ctx := context.Background()
	for _, level := range []ryn.Level{ryn.LevelDebug, ryn.LevelInfo, ryn.LevelWarn, ryn.LevelError} {
		got := ryn.GetLogger().Enabled(ctx, level)
		want := slog.Default().Enabled(ctx, slog.Level(level))
		if got != want {
			t.Errorf("level %v: GetLogger().Enabled=%v, slog.Default().Enabled=%v",
				level, got, want)
		}
	}
}

func TestResetLogger(t *testing.T) {
	orig := ryn.GetLogger()
	t.Cleanup(func() { ryn.SetLogger(orig) })

	// Install discard (definitely non-default behaviour).
	ryn.SetLogger(ryn.Discard())

	// Reset must restore live slog.Default delegation.
	ryn.ResetLogger()

	ctx := context.Background()
	for _, level := range []ryn.Level{ryn.LevelDebug, ryn.LevelInfo, ryn.LevelWarn, ryn.LevelError} {
		got := ryn.GetLogger().Enabled(ctx, level)
		want := slog.Default().Enabled(ctx, slog.Level(level))
		if got != want {
			t.Errorf("after ResetLogger, level %v: Enabled=%v, want %v",
				level, got, want)
		}
	}
}

// ── Package-level log helpers ─────────────────────────────────────────────────

func testLogger(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	l := ryn.NewSlogAdapter(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	ryn.SetLogger(l)
	return &buf, func() { ryn.ResetLogger() }
}

func TestLogWarnEmits(t *testing.T) {
	buf, cleanup := testLogger(t)
	defer cleanup()

	ryn.LogWarn(context.Background(), "retry-event", "attempt", 2, "delay", "200ms")
	got := buf.String()
	if !strings.Contains(got, "retry-event") {
		t.Errorf("expected 'retry-event' in output, got: %s", got)
	}
	if !strings.Contains(got, "attempt=2") {
		t.Errorf("expected 'attempt=2' in output, got: %s", got)
	}
}

func TestLogDebugDisabledIsNoop(t *testing.T) {
	// Install a logger that only passes Warn+ so Debug is disabled.
	var buf bytes.Buffer
	l := ryn.NewSlogAdapter(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))
	ryn.SetLogger(l)
	defer ryn.ResetLogger()

	ryn.LogDebug(context.Background(), "should-not-appear", "k", "v")
	if buf.Len() != 0 {
		t.Errorf("disabled LogDebug must produce no output, got: %s", buf.String())
	}
}

func TestLogErrorEmits(t *testing.T) {
	buf, cleanup := testLogger(t)
	defer cleanup()

	ryn.LogError(context.Background(), "auth-failure", "provider", "openai")
	got := buf.String()
	if !strings.Contains(got, "auth-failure") {
		t.Errorf("expected 'auth-failure' in output, got: %s", got)
	}
}

// ── PCI-DSS scrubber ──────────────────────────────────────────────────────────

func TestDefaultScrubber(t *testing.T) {
	cases := []struct {
		key        string
		wantRedact bool
	}{
		// Sensitive — must be redacted.
		{"api_key", true},
		{"API_KEY", true}, // case-insensitive
		{"authorization", true},
		{"Authorization", true},
		{"bearer_token", true},
		{"password", true},
		{"passwd", true},
		{"my_secret", true},
		{"auth_header", true},
		{"token", true},
		{"credential", true},
		{"pan", true},
		{"card_number", true},
		{"cvv", true},
		{"cvc", true},
		{"ssn", true},
		// Safe — must pass through unchanged.
		{"attempt", false},
		{"delay", false},
		{"provider", false},
		{"model", false},
		{"request_id", false},
		{"http_status", false},
		{"retryable", false},
	}
	for _, c := range cases {
		got := ryn.DefaultScrubber(c.key, "sensitive-value")
		redacted := got == "[REDACTED]"
		if redacted != c.wantRedact {
			t.Errorf("DefaultScrubber(%q): redacted=%v, want %v", c.key, redacted, c.wantRedact)
		}
	}
}

func TestSetScrubberMasksSensitiveFields(t *testing.T) {
	buf, cleanup := testLogger(t)
	defer cleanup()

	ryn.SetScrubber(ryn.DefaultScrubber)
	defer ryn.SetScrubber(nil)

	ryn.LogWarn(context.Background(), "auth-audit",
		"api_key", "sk-should-be-redacted",
		"attempt", 1,
	)

	out := buf.String()
	if strings.Contains(out, "sk-should-be-redacted") {
		t.Error("scrubber must mask the api_key value; found plaintext in output")
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output, got: %s", out)
	}
	// Safe field must be present unchanged.
	if !strings.Contains(out, "attempt=1") {
		t.Errorf("safe field 'attempt=1' should appear in output, got: %s", out)
	}
}

func TestSetScrubberNilRemoves(t *testing.T) {
	ryn.SetScrubber(ryn.DefaultScrubber)
	ryn.SetScrubber(nil) // must not panic

	buf, cleanup := testLogger(t)
	defer cleanup()

	// Without a scrubber the raw value passes through.
	ryn.LogWarn(context.Background(), "no-scrub", "api_key", "visible-value")
	if !strings.Contains(buf.String(), "visible-value") {
		t.Error("with no scrubber, raw value should appear in output")
	}
}

func TestScrubberOnlyRunsWhenEnabled(t *testing.T) {
	// Scrubber must not be called when the level is disabled.
	called := false
	ryn.SetScrubber(func(key string, val any) any {
		called = true
		return val
	})
	defer ryn.SetScrubber(nil)

	ryn.SetLogger(ryn.Discard()) // Discard disables all levels
	defer ryn.ResetLogger()

	ryn.LogWarn(context.Background(), "msg", "k", "v")
	if called {
		t.Error("Scrubber must not be invoked when the logger is disabled")
	}
}

// ── Error.LogValue ────────────────────────────────────────────────────────────

func TestErrorLogValueNilNoPanic(t *testing.T) {
	var e *ryn.Error
	_ = e.LogValue() // must not panic
}

func TestErrorLogValueMinimal(t *testing.T) {
	e := &ryn.Error{Code: ryn.ErrCodeInternalError, Message: "oops"}
	v := e.LogValue()

	if v.Kind() != slog.KindGroup {
		t.Fatalf("Kind = %v; want KindGroup", v.Kind())
	}
	attrs := attrMap(v.Group())

	mustInt(t, attrs, "code", int(ryn.ErrCodeInternalError))
	mustStr(t, attrs, "message", "oops")

	// Optional fields must be absent for a minimal error.
	for _, key := range []string{"provider", "http_status", "retryable", "request_id", "cause"} {
		if _, ok := attrs[key]; ok {
			t.Errorf("unexpected attr %q in minimal error", key)
		}
	}
}

func TestErrorLogValueFull(t *testing.T) {
	cause := errors.New("upstream timeout")
	e := &ryn.Error{
		Code:       ryn.ErrCodeRateLimited,
		Message:    "rate limited",
		Provider:   "openai",
		StatusCode: 429,
		Retryable:  true,
		RequestID:  "req-abc-123",
		Err:        cause,
	}
	v := e.LogValue()

	if v.Kind() != slog.KindGroup {
		t.Fatalf("Kind = %v; want KindGroup", v.Kind())
	}
	attrs := attrMap(v.Group())

	mustInt(t, attrs, "code", int(ryn.ErrCodeRateLimited))
	mustStr(t, attrs, "message", "rate limited")
	mustStr(t, attrs, "provider", "openai")
	mustInt(t, attrs, "http_status", 429)
	mustBool(t, attrs, "retryable", true)
	mustStr(t, attrs, "request_id", "req-abc-123")
	mustStr(t, attrs, "cause", cause.Error())
}

func TestErrorLogValueRetryableOmittedWhenFalse(t *testing.T) {
	e := &ryn.Error{Code: ryn.ErrCodeInvalidRequest, Message: "bad", Retryable: false}
	attrs := attrMap(e.LogValue().Group())
	if _, ok := attrs["retryable"]; ok {
		t.Error("retryable attr should be omitted when false")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func attrMap(attrs []slog.Attr) map[string]slog.Value {
	m := make(map[string]slog.Value, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a.Value
	}
	return m
}

func mustInt(t *testing.T, m map[string]slog.Value, key string, want int) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("missing attr %q", key)
		return
	}
	if got := int(v.Int64()); got != want {
		t.Errorf("attr %q = %d; want %d", key, got, want)
	}
}

func mustStr(t *testing.T, m map[string]slog.Value, key, want string) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("missing attr %q", key)
		return
	}
	if got := v.String(); got != want {
		t.Errorf("attr %q = %q; want %q", key, got, want)
	}
}

func mustBool(t *testing.T, m map[string]slog.Value, key string, want bool) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("missing attr %q", key)
		return
	}
	if got := v.Bool(); got != want {
		t.Errorf("attr %q = %v; want %v", key, got, want)
	}
}
