package ryn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// TimeoutConfig configures timeout behavior.
type TimeoutConfig struct {
	// GenerationTimeout is the max time for a single generation request
	GenerationTimeout time.Duration
	// FrameTimeout is the max time to wait for the next frame
	FrameTimeout time.Duration
	// ToolTimeout is the max time to wait for a tool execution result
	ToolTimeout time.Duration
}

// DefaultTimeoutConfig returns sensible defaults:
// - 5 minutes for generation
// - 30 seconds for each frame
// - 1 minute for tool execution
func DefaultTimeoutConfig() TimeoutConfig {
	return TimeoutConfig{
		GenerationTimeout: 5 * time.Minute,
		FrameTimeout:      30 * time.Second,
		ToolTimeout:       1 * time.Minute,
	}
}

// WithGenerationTimeout returns a context with a generation timeout applied.
func WithGenerationTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, timeout)
}

// TimeoutProvider wraps a Provider with generation timeout enforcement.
type TimeoutProvider struct {
	provider Provider
	timeout  time.Duration
}

// NewTimeoutProvider creates a Provider that enforces generation timeouts.
func NewTimeoutProvider(p Provider, timeout time.Duration) *TimeoutProvider {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &TimeoutProvider{provider: p, timeout: timeout}
}

// Generate implements Provider with timeout enforcement.
func (tp *TimeoutProvider) Generate(ctx context.Context, req *Request) (*Stream, error) {
	ctx, cancel := context.WithTimeout(ctx, tp.timeout)
	defer cancel()
	return tp.provider.Generate(ctx, req)
}

// --- Request ID and Tracing ---

// RequestID is a unique identifier for a request used for tracing.
type RequestID string

// GenerateRequestID creates a new unique request ID.
func GenerateRequestID() RequestID {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback if crypto/rand fails
		for i := range b {
			b[i] = byte(i)
		}
	}
	return RequestID("req_" + hex.EncodeToString(b))
}

// String returns the string representation of the RequestID.
func (rid RequestID) String() string {
	return string(rid)
}

// TraceContext holds tracing information for a generation.
type TraceContext struct {
	RequestID  RequestID              // Unique request ID
	ParentID   string                 // Parent span ID (optional, for distributed tracing)
	UserID     string                 // User identifier (optional)
	SessionID  string                 // Session identifier (optional)
	Attributes map[string]interface{} // Additional attributes
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
	provider Provider
}

// NewTracingProvider creates a Provider that automatically generates and propagates trace IDs.
func NewTracingProvider(p Provider) *TracingProvider {
	return &TracingProvider{provider: p}
}

// Generate implements Provider with automatic trace context injection.
func (tp *TracingProvider) Generate(ctx context.Context, req *Request) (*Stream, error) {
	// Ensure trace context exists
	trace := GetTraceContext(ctx)
	if trace.RequestID == "" {
		trace.RequestID = GenerateRequestID()
		ctx = WithTraceContext(ctx, trace)
	}
	return tp.provider.Generate(ctx, req)
}

// ComposedProvider combines multiple provider wrappers (timeout, tracing, retry).
// Useful for building a production-ready provider from building blocks.
func ComposedProvider(p Provider, timeout time.Duration, retryConfig *RetryConfig) Provider {
	// Apply in order: retry (outermost) → timeout → tracing → base
	var composed Provider = p

	// Innermost: tracing
	composed = NewTracingProvider(composed)

	// Middle: timeout
	if timeout > 0 {
		composed = NewTimeoutProvider(composed, timeout)
	}

	// Outermost: retry
	if retryConfig != nil {
		composed = NewRetryProvider(composed, *retryConfig)
	}

	return composed
}
