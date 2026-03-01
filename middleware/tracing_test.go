package middleware_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/middleware"
)

func TestGenerateRequestID(t *testing.T) {
	t.Parallel()

	id1 := middleware.GenerateRequestID()
	id2 := middleware.GenerateRequestID()

	assertTrue(t, len(id1.String()) > 0)
	assertTrue(t, id1.String() != id2.String())
	assertTrue(t, strings.HasPrefix(id1.String(), "req_"))
}

func TestTraceContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	trace := middleware.TraceContext{
		RequestID: middleware.GenerateRequestID(),
		UserID:    "user123",
		SessionID: "session456",
	}

	ctx = middleware.WithTraceContext(ctx, trace)
	retrieved := middleware.GetTraceContext(ctx)

	assertEqual(t, retrieved.RequestID, trace.RequestID)
	assertEqual(t, retrieved.UserID, "user123")
	assertEqual(t, retrieved.SessionID, "session456")
}

func TestTracingProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		trace := middleware.GetTraceContext(ctx)
		assertTrue(t, trace.RequestID != "")
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	provider := middleware.NewTracingProvider(mock)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, stream)
	assertEqual(t, text, "ok")
}

func TestWithTraceContextEmptyRequestID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Empty RequestID → WithTraceContext auto-generates one.
	trace := middleware.TraceContext{UserID: "user42"} // RequestID is zero-value ""
	ctx = middleware.WithTraceContext(ctx, trace)

	retrieved := middleware.GetTraceContext(ctx)
	assertTrue(t, retrieved.RequestID != "")
	assertEqual(t, retrieved.UserID, "user42")
}

func TestGetTraceContextNoTrace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// No trace in context → GetTraceContext creates a new one with generated ID.
	trace := middleware.GetTraceContext(ctx)
	assertTrue(t, trace.RequestID != "")
}

func TestTracingProviderPreservesExistingTrace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	existingID := middleware.GenerateRequestID()
	ctx = middleware.WithTraceContext(ctx, middleware.TraceContext{
		RequestID: existingID,
		UserID:    "u1",
	})

	var receivedID middleware.RequestID
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		trace := middleware.GetTraceContext(ctx)
		receivedID = trace.RequestID
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	provider := middleware.NewTracingProvider(mock)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertNoError(t, err)
	ryn.CollectText(ctx, stream)

	// Existing trace should be preserved.
	assertEqual(t, receivedID, existingID)
}

func TestTracingProviderInjectsTraceIntoCtx(t *testing.T) {
	t.Parallel()
	ctx := context.Background() // no trace

	// Verify the trace context is stored in the ctx passed to the downstream
	// provider, so derived child contexts inherit the same RequestID.
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		// GetTraceContext on the received ctx should return the stored trace.
		t1 := middleware.GetTraceContext(ctx)
		// Create a derived context (simulating downstream middleware).
		derived, cancel := context.WithCancel(ctx)
		defer cancel()
		t2 := middleware.GetTraceContext(derived)
		if t1.RequestID != t2.RequestID {
			t.Errorf("trace not stored in ctx: parent=%v child=%v", t1.RequestID, t2.RequestID)
		}
		assertTrue(t, t1.RequestID != "")
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	provider := middleware.NewTracingProvider(mock)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, stream)
	assertEqual(t, text, "ok")
}

func TestTracingProviderUserIDPreserved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ctx = middleware.WithTraceContext(ctx, middleware.TraceContext{
		RequestID: middleware.GenerateRequestID(),
		UserID:    "user-42",
		SessionID: "sess-1",
	})

	var gotUID, gotSID string
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		trace := middleware.GetTraceContext(ctx)
		gotUID = trace.UserID
		gotSID = trace.SessionID
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	provider := middleware.NewTracingProvider(mock)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("x")}})
	assertNoError(t, err)
	ryn.CollectText(ctx, stream)
	assertNoError(t, err)

	assertEqual(t, gotUID, "user-42")
	assertEqual(t, gotSID, "sess-1")
}

func TestTracingProviderNilProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// TracingProvider wraps a nil provider — Generate should return an error
	// or panic gracefully (depends on downstream). We test that TracingProvider
	// itself doesn't panic and that the error propagates.
	var panicked bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		tp := middleware.NewTracingProvider(ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
			return nil, fmt.Errorf("downstream error")
		}))
		_, err := tp.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("x")}})
		assertTrue(t, err != nil)
	}()
	assertTrue(t, !panicked)
}
