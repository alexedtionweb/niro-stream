package compat_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ryn.dev/ryn"
	"ryn.dev/ryn/provider/compat"
)

func TestCompatProviderStreaming(t *testing.T) {
	t.Parallel()

	// Simulate an OpenAI-compatible SSE endpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected auth: %s", r.Header.Get("Authorization"))
		}

		// Decode the request body to verify
		var body map[string]any
		_ = ryn.JSONNewDecoder(r.Body).Decode(&body)
		if body["model"] != "test-model" {
			t.Errorf("unexpected model: %v", body["model"])
		}
		if body["stream"] != true {
			t.Errorf("expected stream=true")
		}

		// Send SSE response
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		chunks := []string{
			`{"id":"resp1","model":"test-model","choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`{"id":"resp1","model":"test-model","choices":[{"delta":{"content":" World"},"finish_reason":null}]}`,
			`{"id":"resp1","model":"test-model","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		}

		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	ctx := context.Background()
	llm := compat.New(srv.URL, "test-key", compat.WithModel("test-model"))

	stream, err := llm.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("Hi")},
	})
	if err != nil {
		t.Fatal(err)
	}

	text, err := ryn.CollectText(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hello World" {
		t.Errorf("got %q, want %q", text, "Hello World")
	}

	// Check usage
	usage := stream.Usage()
	if usage.InputTokens != 10 {
		t.Errorf("input tokens: got %d, want 10", usage.InputTokens)
	}
	if usage.TotalTokens != 15 {
		t.Errorf("total tokens: got %d, want 15", usage.TotalTokens)
	}

	// Check response meta
	resp := stream.Response()
	if resp == nil {
		t.Fatal("expected response meta")
	}
	if resp.FinishReason != "stop" {
		t.Errorf("finish reason: got %q, want %q", resp.FinishReason, "stop")
	}
	if resp.ID != "resp1" {
		t.Errorf("response ID: got %q, want %q", resp.ID, "resp1")
	}
}

func TestCompatProviderToolCalls(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		chunks := []string{
			// Start of tool call
			`{"id":"resp2","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
			// Arguments streaming
			`{"id":"resp2","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
			`{"id":"resp2","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"NYC\"}"}}]},"finish_reason":null}]}`,
			// Done
			`{"id":"resp2","model":"m","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		}

		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	ctx := context.Background()
	llm := compat.New(srv.URL, "")

	stream, err := llm.Generate(ctx, &ryn.Request{
		Model:    "m",
		Messages: []ryn.Message{ryn.UserText("Weather in NYC?")},
	})
	if err != nil {
		t.Fatal(err)
	}

	frames, err := ryn.Collect(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}

	// Should have one tool call frame
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if frames[0].Kind != ryn.KindToolCall {
		t.Fatalf("expected tool_call, got %s", frames[0].Kind)
	}
	if frames[0].Tool.Name != "get_weather" {
		t.Errorf("tool name: got %q, want %q", frames[0].Tool.Name, "get_weather")
	}
	if frames[0].Tool.ID != "call_1" {
		t.Errorf("tool ID: got %q, want %q", frames[0].Tool.ID, "call_1")
	}

	var args map[string]string
	_ = ryn.JSONUnmarshal(frames[0].Tool.Args, &args)
	if args["city"] != "NYC" {
		t.Errorf("tool args city: got %q, want %q", args["city"], "NYC")
	}
}

func TestCompatProviderErrorStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	llm := compat.New(srv.URL, "")

	_, err := llm.Generate(ctx, &ryn.Request{
		Model:    "m",
		Messages: []ryn.Message{ryn.UserText("hi")},
	})

	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("expected 429 in error, got: %v", err)
	}
}

func TestCompatProviderCustomHeaders(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Custom") != "value" {
			t.Errorf("custom header missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ctx := context.Background()
	llm := compat.New(srv.URL, "", compat.WithHeader("X-Custom", "value"))

	stream, err := llm.Generate(ctx, &ryn.Request{
		Model:    "m",
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	ryn.Collect(ctx, stream) // drain
}
