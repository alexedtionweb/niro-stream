package ryn

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
)

// ── Log levels ────────────────────────────────────────────────────────────────

// Level is the severity of a log record.
//
// Values are intentionally identical to [slog.Level] so adapters convert
// between the two types with a zero-cost integer cast: slog.Level(rynLevel).
//
// PCI-DSS guidance for each level is documented on each constant below.
type Level int

const (
	// LevelDebug is for verbose, per-request diagnostics.
	//
	// PCI-DSS Requirement 10: DEBUG MUST NOT be enabled in production or any
	// PCI-scoped environment. Debug output may include request or response
	// payloads that could expose cardholder data or authentication credentials.
	LevelDebug Level = -4

	// LevelInfo records normal operational events (provider selection, model
	// routing). Safe for production.
	LevelInfo Level = 0

	// LevelWarn records transient, recoverable conditions: retries, rate-limit
	// back-off, degraded provider state. Always safe for production.
	//
	// PCI-DSS Requirement 10.2: repeated warn events are audit signals; ensure
	// your log aggregator retains them for the required 12-month period.
	LevelWarn Level = 4

	// LevelError records unrecoverable failures requiring operator attention.
	// Always safe for production.
	//
	// PCI-DSS Requirement 10.2.4: authentication failures MUST be logged at
	// this level or above.
	LevelError Level = 8
)

// ── Logger interface ──────────────────────────────────────────────────────────

// Logger is the minimal structured-logging interface used internally by ryn.
// Any logging backend can be adapted by implementing two methods.
//
// Key-value pairs in args follow the slog alternating-key-value convention
// (string, any, string, any, …). ryn's own call sites use only plain string
// keys and standard Go values — no slog.Attr — so non-slog adapters require
// no special-case handling.
//
// Implementations MUST be safe for concurrent use.
//
// # PCI-DSS guarantee
//
// ryn only ever passes pre-approved, non-sensitive attributes to the logger:
// error codes, retry counts, durations, and request IDs. Request/response
// content and credentials are NEVER passed to the logger. Install a [Scrubber]
// as defence-in-depth if your application extends ryn log call sites.
//
// # Adapter recipes
//
//	// slog (built-in, zero boilerplate):
//	ryn.SetLogger(ryn.NewSlogAdapter(slog.Default()))
//
//	// zap:
//	ryn.SetLogger(&zapAdapter{l: zapLogger})
//	// Enabled: l.Core().Enabled(zapcore.Level(level+4))
//	// Log:     l.Sugar().Log(zapcore.Level(level+4), msg, keysAndValues...)
//
//	// zerolog:
//	ryn.SetLogger(&zerologAdapter{l: &zerologLogger})
//	// Enabled: l.GetLevel() <= zerolog.Level(level/4+1)
//	// Log:     l.WithLevel(...).Fields(args).Msg(msg)
type Logger interface {
	// Enabled reports whether records at level would be processed.
	// MUST be cheap: no allocation, no lock, branch-predictor friendly.
	// ryn calls Enabled before constructing expensive args; a false return
	// is a guaranteed zero-cost exit from the log helper.
	Enabled(ctx context.Context, level Level) bool

	// Log emits a structured record. ryn guarantees Enabled(ctx,level)==true.
	// args is a flat alternating (string key, any value) list.
	Log(ctx context.Context, level Level, msg string, args ...any)
}

// ── Storage ───────────────────────────────────────────────────────────────────

// logBox is an atomic.Value-safe carrier.
// A nil l field means "delegate live to slog.Default()".
type logBox struct{ l Logger }

var logStore atomic.Value // always stores logBox; zero value has nil l

func init() { logStore.Store(logBox{}) } // nil l = live slog.Default delegation

// GetLogger returns the active library Logger.
//
// Before any [SetLogger] call (or after [ResetLogger]) this returns a live
// adapter over [slog.Default]: changes applied via [slog.SetDefault] are
// automatically visible here with no extra configuration.
func GetLogger() Logger {
	b := logStore.Load().(logBox)
	if b.l != nil {
		return b.l
	}
	return newSlogAdapter(slog.Default()) // live: re-evaluated on every call
}

// SetLogger replaces the library-wide logger shared by all ryn packages.
// It is safe to call concurrently at any time.
//
// Pass nil to install [Discard] (suppress all output).
// Call [ResetLogger] to restore live [slog.Default] delegation.
//
//	// JSON output at debug level (use only in non-PCI environments):
//	ryn.SetLogger(ryn.NewSlogAdapter(slog.New(
//	    slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}),
//	)))
//
//	// Suppress all output:
//	ryn.SetLogger(ryn.Discard())
//
//	// Custom backend (zap, zerolog, …):
//	ryn.SetLogger(&myZapAdapter{l: zapLogger})
func SetLogger(l Logger) {
	if l == nil {
		l = Discard()
	}
	logStore.Store(logBox{l})
}

// ResetLogger removes any override installed by [SetLogger], restoring live
// delegation to [slog.Default].
func ResetLogger() {
	logStore.Store(logBox{}) // nil l = live slog.Default delegation
}

// ── slog adapter ─────────────────────────────────────────────────────────────

type slogAdapter struct{ l *slog.Logger }

// NewSlogAdapter wraps a *slog.Logger as a ryn [Logger].
// The Level↔slog.Level conversion is a zero-cost integer cast (same values).
// If l is nil, [Discard] is returned.
func NewSlogAdapter(l *slog.Logger) Logger {
	if l == nil {
		return Discard()
	}
	return slogAdapter{l}
}

func newSlogAdapter(l *slog.Logger) Logger { return slogAdapter{l} }

func (a slogAdapter) Enabled(ctx context.Context, level Level) bool {
	return a.l.Enabled(ctx, slog.Level(level))
}

func (a slogAdapter) Log(ctx context.Context, level Level, msg string, args ...any) {
	a.l.Log(ctx, slog.Level(level), msg, args...)
}

// ── Discard logger ────────────────────────────────────────────────────────────

type discardLogger struct{}

// discardSingleton is pre-allocated so Discard() is always zero-alloc.
var discardSingleton Logger = discardLogger{}

// Discard returns a [Logger] that silently drops all records.
// Enabled always returns false so callers never build args.
//
//	ryn.SetLogger(ryn.Discard())
func Discard() Logger { return discardSingleton }

func (discardLogger) Enabled(_ context.Context, _ Level) bool            { return false }
func (discardLogger) Log(_ context.Context, _ Level, _ string, _ ...any) {}

// ── DiscardHandler (slog.Handler) ─────────────────────────────────────────────

// DiscardHandler is a [slog.Handler] that silently discards all records.
// Use it when you need a silent *slog.Logger for [NewSlogAdapter]:
//
//	ryn.SetLogger(ryn.NewSlogAdapter(slog.New(ryn.DiscardHandler{})))
//
// For most cases, prefer [Discard] directly.
type DiscardHandler struct{}

func (DiscardHandler) Enabled(_ context.Context, _ slog.Level) bool  { return false }
func (DiscardHandler) Handle(_ context.Context, _ slog.Record) error { return nil }
func (h DiscardHandler) WithAttrs(_ []slog.Attr) slog.Handler        { return h }
func (h DiscardHandler) WithGroup(_ string) slog.Handler             { return h }

// ── Package-level log helpers ─────────────────────────────────────────────────
//
// Each helper checks Enabled before any work is done. This guarantees that
// disabled levels are zero-cost after a single atomic load + Enabled call.
//
// For truly zero-allocation guards around expensive arg construction:
//
//	if l := ryn.GetLogger(); l.Enabled(ctx, ryn.LevelDebug) {
//	    l.Log(ctx, ryn.LevelDebug, "msg", "k", expensiveValue())
//	}

// LogDebug emits a DEBUG record. Zero overhead when DEBUG is disabled.
//
// PCI-DSS: DEBUG MUST NOT be enabled in production or PCI-scoped environments.
func LogDebug(ctx context.Context, msg string, args ...any) { logAt(ctx, LevelDebug, msg, args) }

// LogInfo emits an INFO record. Safe for production.
func LogInfo(ctx context.Context, msg string, args ...any) { logAt(ctx, LevelInfo, msg, args) }

// LogWarn emits a WARN record for transient failures and retries.
//
// PCI-DSS Req 10.2: warn events are audit signals; retain logs for 12 months.
func LogWarn(ctx context.Context, msg string, args ...any) { logAt(ctx, LevelWarn, msg, args) }

// LogError emits an ERROR record for failures requiring operator attention.
//
// PCI-DSS Req 10.2.4: authentication failures are always emitted at ERROR.
func LogError(ctx context.Context, msg string, args ...any) { logAt(ctx, LevelError, msg, args) }

// logAt is the shared implementation. Receives args as []any (not variadic)
// so the package-level helpers pass the already-created slice without a copy.
func logAt(ctx context.Context, level Level, msg string, args []any) {
	l := GetLogger()
	if !l.Enabled(ctx, level) {
		return // fast-path: zero work beyond Enabled check
	}
	if sp := globalScrubber.Load(); sp != nil {
		args = scrubArgs(*sp, args)
	}
	l.Log(ctx, level, msg, args...)
}

// ── PCI-DSS scrubber ──────────────────────────────────────────────────────────

// Scrubber sanitises log attribute values before they reach the Logger.
//
// PCI-DSS Requirements 3.4 and 10.3 prohibit logging PANs, credentials, and
// authentication tokens in clear text. Install a Scrubber as a
// defence-in-depth layer. ryn itself never passes sensitive values to the
// logger; the Scrubber protects against application code that adds attrs to
// ryn log call sites.
//
// The function receives each attribute key and value; it returns the value to
// emit, a masked replacement such as "[REDACTED]", or nil to drop the field.
//
// Install via [SetScrubber]. [DefaultScrubber] covers the most common PCI-DSS
// sensitive key patterns. The scrubber is only invoked when the logger is
// enabled for the record's level — zero overhead when the level is off.
type Scrubber func(key string, val any) any

var globalScrubber atomic.Pointer[Scrubber]

// SetScrubber installs a global [Scrubber] applied to all ryn log helpers.
// Pass nil to remove the installed scrubber (default: none).
//
//	ryn.SetScrubber(ryn.DefaultScrubber)
func SetScrubber(s Scrubber) {
	if s == nil {
		globalScrubber.Store(nil)
		return
	}
	globalScrubber.Store(&s)
}

// DefaultScrubber redacts values whose keys contain substrings associated with
// authentication material or cardholder data, satisfying PCI-DSS
// Requirements 3.4 and 10.3. Key matching is case-insensitive substring.
//
// Redacted key patterns:
//
//	authorization, api_key, apikey, password, passwd, secret,
//	token, credential, auth, bearer, pan, card, cvv, cvc, ssn.
func DefaultScrubber(key string, val any) any {
	k := strings.ToLower(key)
	for _, p := range pciSensitiveKeys {
		if strings.Contains(k, p) {
			return "[REDACTED]"
		}
	}
	return val
}

// pciSensitiveKeys are the substring patterns used by [DefaultScrubber].
var pciSensitiveKeys = [...]string{
	"authorization", "api_key", "apikey", "password", "passwd",
	"secret", "token", "credential", "auth", "bearer",
	"pan", "card", "cvv", "cvc", "ssn",
}

// scrubArgs returns a copy of args with each value run through s.
// Only called when a Scrubber is installed and the level is enabled.
func scrubArgs(s Scrubber, args []any) []any {
	out := make([]any, len(args))
	copy(out, args)
	for i := 0; i+1 < len(out); i += 2 {
		if key, ok := out[i].(string); ok {
			out[i+1] = s(key, out[i+1])
		}
	}
	return out
}
