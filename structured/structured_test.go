package structured_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/structured"
)

func TestGenerateStructured(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type result struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"]}`)

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		if req.ResponseFormat != "json_schema" {
			t.Errorf("expected ResponseFormat=json_schema, got %q", req.ResponseFormat)
		}
		if string(req.ResponseSchema) != string(schema) {
			t.Errorf("schema mismatch")
		}
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.TextFrame(`{"name":"alice","age":30}`))
			_ = e.Emit(ctx, ryn.UsageFrame(&ryn.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}))
		}()
		return s, nil
	})

	res, _, usage, err := structured.GenerateStructured[result](ctx, mock, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	}, schema)
	assertNoError(t, err)
	assertEqual(t, res.Name, "alice")
	assertEqual(t, res.Age, 30)
	assertEqual(t, usage.TotalTokens, 8)
}

func TestStreamStructuredPartialAndFinal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type result struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"]}`)

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.TextFrame(`{"name":"al`))
			_ = e.Emit(ctx, ryn.TextFrame(`ice","age":30}`))
		}()
		return s, nil
	})

	ss, err := structured.StreamStructured[result](ctx, mock, &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}}, schema)
	assertNoError(t, err)

	partialSeen := false
	finalSeen := false
	for ss.Next(ctx) {
		ev := ss.Event()
		if ev.Partial != nil {
			partialSeen = true
		}
		if ev.Final != nil {
			finalSeen = true
			assertEqual(t, ev.Final.Name, "alice")
			assertEqual(t, ev.Final.Age, 30)
		}
	}
	assertTrue(t, partialSeen)
	assertTrue(t, finalSeen)
	assertNoError(t, ss.Err())
}

func TestGenerateStructuredNoText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	schema := json.RawMessage(`{"type":"object"}`)
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.UsageFrame(&ryn.Usage{InputTokens: 1}))
		}()
		return s, nil
	})

	_, _, _, err := structured.GenerateStructured[map[string]any](ctx, mock, &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}}, schema)
	assertErrorContains(t, err, "no structured output")
}

func TestWithSchemaAny(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type result struct {
		Value string `json:"value"`
	}

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": map[string]any{"type": "string"},
		},
	}

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		if req.ResponseFormat != "json_schema" {
			t.Errorf("expected json_schema ResponseFormat, got %q", req.ResponseFormat)
		}
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.TextFrame(`{"value":"hello"}`))
		}()
		return s, nil
	})

	req, err := structured.WithSchemaAny(&ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	}, schema)
	assertNoError(t, err)

	ss, err := structured.StreamStructured[result](ctx, mock, req, req.ResponseSchema)
	assertNoError(t, err)

	var finalResult *result
	for ss.Next(ctx) {
		ev := ss.Event()
		if ev.Final != nil {
			finalResult = ev.Final
		}
	}
	assertNoError(t, ss.Err())
	if finalResult == nil {
		t.Fatal("expected final result")
	}
	assertEqual(t, finalResult.Value, "hello")
}

func TestWithSchemaAnyMarshalError(t *testing.T) {
	t.Parallel()

	// channel cannot be marshaled to JSON
	_, err := structured.WithSchemaAny(&ryn.Request{}, make(chan int))
	assertTrue(t, err != nil)
}

func TestStreamStructuredUsageAndResponse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type result struct {
		X int `json:"x"`
	}

	schema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}}}`)
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.TextFrame(`{"x":42}`))
			_ = e.Emit(ctx, ryn.UsageFrame(&ryn.Usage{InputTokens: 2, OutputTokens: 4, TotalTokens: 6}))
			e.SetResponse(&ryn.ResponseMeta{Model: "test-model", FinishReason: "stop"})
		}()
		return s, nil
	})

	ss, err := structured.StreamStructured[result](ctx, mock, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	}, schema)
	assertNoError(t, err)

	for ss.Next(ctx) {
	}
	assertNoError(t, ss.Err())

	usage := ss.Usage()
	assertEqual(t, usage.TotalTokens, 6)

	resp := ss.Response()
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	assertEqual(t, resp.Model, "test-model")
}

func TestStreamStructuredStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type result struct{ X int }
	schema := json.RawMessage(`{"type":"object"}`)

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.TextFrame(`{"x":1`)) // partial, invalid JSON
			e.Error(fmt.Errorf("stream error"))
		}()
		return s, nil
	})

	ss, err := structured.StreamStructured[result](ctx, mock, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	}, schema)
	assertNoError(t, err)

	for ss.Next(ctx) {
	}
	assertTrue(t, ss.Err() != nil)
}

func TestStreamStructuredNoOutput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type result struct{ X int }
	schema := json.RawMessage(`{"type":"object"}`)

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			// emit no text
		}()
		return s, nil
	})

	ss, err := structured.StreamStructured[result](ctx, mock, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	}, schema)
	assertNoError(t, err)

	finalSeen := false
	for ss.Next(ctx) {
		finalSeen = true
	}
	assertTrue(t, !finalSeen)
	assertTrue(t, ss.Err() != nil)
}

func TestGenerateStructuredProviderError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	schema := json.RawMessage(`{"type":"object"}`)
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, fmt.Errorf("provider failed")
	})

	_, _, _, err := structured.GenerateStructured[map[string]any](ctx, mock, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	}, schema)
	assertTrue(t, err != nil)
}

func TestStreamStructuredProviderError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	schema := json.RawMessage(`{"type":"object"}`)
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, fmt.Errorf("provider failed")
	})

	_, err := structured.StreamStructured[map[string]any](ctx, mock, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	}, schema)
	assertTrue(t, err != nil)
}

func TestGenerateStructuredBadJSON(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	schema := json.RawMessage(`{"type":"object"}`)
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.TextFrame(`{not valid json}`))
		}()
		return s, nil
	})

	_, _, _, err := structured.GenerateStructured[map[string]any](ctx, mock, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	}, schema)
	assertTrue(t, err != nil)
}

// --- test helpers ---

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
	found := false
	for i := 0; i <= len(err.Error())-len(substr); i++ {
		if err.Error()[i:i+len(substr)] == substr {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("error %q does not contain %q", err.Error(), substr)
	}
}

func assertTrue(t *testing.T, v bool) {
	t.Helper()
	if !v {
		t.Error("expected true")
	}
}
