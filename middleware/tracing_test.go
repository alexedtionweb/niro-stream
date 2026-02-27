package middleware_test

import (
	"context"
	"strings"
	"testing"

	"ryn.dev/ryn"
	"ryn.dev/ryn/middleware"
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
