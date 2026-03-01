package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

// RequestID is a unique identifier for a request used for tracing.
type RequestID string

// requestCounter is a monotonic fallback counter used only when crypto/rand
// is unavailable.
var requestCounter atomic.Uint64

// GenerateRequestID creates a new unique request ID using crypto/rand.
func GenerateRequestID() RequestID {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		n := requestCounter.Add(1)
		ts := time.Now().UnixNano()
		return RequestID(fmt.Sprintf("req_%x_%x", ts, n))
	}
	return RequestID("req_" + hex.EncodeToString(b))
}

// String returns the string representation of the RequestID.
func (rid RequestID) String() string {
	return string(rid)
}

// TraceContext holds tracing information for a generation.
type TraceContext struct {
	RequestID  RequestID         // Unique request ID
	ParentID   string            // Parent span ID (optional)
	UserID     string            // User identifier (optional)
	SessionID  string            // Session identifier (optional)
	Attributes map[string]string // Additional attributes
}

// WithTraceContext injects trace context into a request context.
func WithTraceContext(ctx context.Context, trace TraceContext) context.Context {
	if trace.RequestID == "" {
		trace.RequestID = GenerateRequestID()
	}
	return context.WithValue(ctx, traceContextKey{}, trace)
}

// GetTraceContext retrieves trace context from a request context.
func GetTraceContext(ctx context.Context) TraceContext {
	trace, ok := ctx.Value(traceContextKey{}).(TraceContext)
	if !ok {
		trace = TraceContext{RequestID: GenerateRequestID()}
	}
	return trace
}

type traceContextKey struct{}

// TracingProvider wraps a Provider and injects tracing information.
type TracingProvider struct {
	provider niro.Provider
}

// NewTracingProvider creates a Provider that automatically generates
// and propagates trace IDs.
func NewTracingProvider(p niro.Provider) *TracingProvider {
	return &TracingProvider{provider: p}
}

// Generate implements Provider with automatic trace context injection.
func (tp *TracingProvider) Generate(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
	// GetTraceContext always returns a non-empty RequestID (auto-generated
	// when absent). Always store it back so the downstream provider sees it.
	trace := GetTraceContext(ctx)
	ctx = WithTraceContext(ctx, trace)
	return tp.provider.Generate(ctx, req)
}
