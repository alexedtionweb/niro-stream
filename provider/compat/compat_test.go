package compat_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/provider/compat"
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
		_ = niro.JSONNewDecoder(r.Body).Decode(&body)
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

	stream, err := llm.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("Hi")},
	})
	if err != nil {
		t.Fatal(err)
	}

	text, err := niro.CollectText(ctx, stream)
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

	stream, err := llm.Generate(ctx, &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("Weather in NYC?")},
	})
	if err != nil {
		t.Fatal(err)
	}

	frames, err := niro.Collect(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}

	// Should have one tool call frame
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if frames[0].Kind != niro.KindToolCall {
		t.Fatalf("expected tool_call, got %s", frames[0].Kind)
	}
	if frames[0].Tool.Name != "get_weather" {
		t.Errorf("tool name: got %q, want %q", frames[0].Tool.Name, "get_weather")
	}
	if frames[0].Tool.ID != "call_1" {
		t.Errorf("tool ID: got %q, want %q", frames[0].Tool.ID, "call_1")
	}

	var args map[string]string
	_ = niro.JSONUnmarshal(frames[0].Tool.Args, &args)
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

	_, err := llm.Generate(ctx, &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("hi")},
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

	stream, err := llm.Generate(ctx, &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	niro.Collect(ctx, stream) // drain
}

func TestCompatWithClient(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	customClient := &http.Client{}
	llm := compat.New(srv.URL, "", compat.WithClient(customClient))

	ctx := context.Background()
	stream, err := llm.Generate(ctx, &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	niro.Collect(ctx, stream)
}

func TestCompatProviderMultipartMessages(t *testing.T) {
	t.Parallel()

	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		niro.JSONNewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ctx := context.Background()
	llm := compat.New(srv.URL, "")

	// Message with image URL
	stream, err := llm.Generate(ctx, &niro.Request{
		Model: "m",
		Messages: []niro.Message{
			niro.Multi(niro.RoleUser,
				niro.TextPart("check this"),
				niro.ImageURLPart("https://example.com/img.png", "image/png"),
			),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	niro.Collect(ctx, stream)
}

func TestCompatProviderImageDataMessage(t *testing.T) {
	t.Parallel()

	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		niro.JSONNewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ctx := context.Background()
	llm := compat.New(srv.URL, "")

	stream, err := llm.Generate(ctx, &niro.Request{
		Model: "m",
		Messages: []niro.Message{
			niro.Multi(niro.RoleUser,
				niro.TextPart("check this"),
				niro.ImagePart([]byte{0xFF, 0xD8}, "image/jpeg"),
			),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	niro.Collect(ctx, stream)
}

func TestCompatProviderAudioMessage(t *testing.T) {
	t.Parallel()

	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		niro.JSONNewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ctx := context.Background()
	llm := compat.New(srv.URL, "")

	// Test each audio format
	for _, mime := range []string{"audio/mpeg", "audio/opus", "audio/flac", "audio/wav"} {
		stream, err := llm.Generate(ctx, &niro.Request{
			Model: "m",
			Messages: []niro.Message{
				niro.Multi(niro.RoleUser,
					niro.AudioPart([]byte{0x01}, mime),
				),
			},
		})
		if err != nil {
			t.Fatalf("mime %s: %v", mime, err)
		}
		niro.Collect(ctx, stream)
	}
}

func TestCompatProviderToolCallMessage(t *testing.T) {
	t.Parallel()

	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		niro.JSONNewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ctx := context.Background()
	llm := compat.New(srv.URL, "")

	// Assistant with tool call
	stream, err := llm.Generate(ctx, &niro.Request{
		Model: "m",
		Messages: []niro.Message{
			niro.UserText("what's the weather?"),
			niro.Multi(niro.RoleAssistant,
				niro.ToolCallPart(&niro.ToolCall{ID: "c1", Name: "get_weather", Args: []byte(`{"city":"NYC"}`)}),
			),
			niro.ToolMessage("c1", `{"temp":72}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	niro.Collect(ctx, stream)
}

func TestCompatProviderWithOptions(t *testing.T) {
	t.Parallel()

	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		niro.JSONNewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ctx := context.Background()
	temp := 0.7
	topP := 0.9
	llm := compat.New(srv.URL, "")

	stream, err := llm.Generate(ctx, &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("hi")},
		Options: niro.Options{
			MaxTokens:   100,
			Temperature: &temp,
			TopP:        &topP,
			Stop:        []string{"END"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	niro.Collect(ctx, stream)
}

func TestCompatProviderWithTools(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ctx := context.Background()
	llm := compat.New(srv.URL, "")

	stream, err := llm.Generate(ctx, &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("hi")},
		Tools: []niro.Tool{{
			Name:        "get_weather",
			Description: "Get weather",
			Parameters:  []byte(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	niro.Collect(ctx, stream)
}

func TestCompatSSEParseError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// Send invalid JSON
		fmt.Fprint(w, "data: {invalid json}\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	ctx := context.Background()
	llm := compat.New(srv.URL, "")

	stream, err := llm.Generate(ctx, &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Collect should return an error due to invalid JSON
	_, err = niro.Collect(ctx, stream)
	if err == nil {
		t.Error("expected error from invalid SSE JSON")
	}
}

func TestCompatUsageOnlyChunk(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		// Chunk with no choices but has usage (stream_options style)
		fmt.Fprint(w, `data: {"id":"r1","model":"m","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	ctx := context.Background()
	llm := compat.New(srv.URL, "")

	stream, err := llm.Generate(ctx, &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	niro.Collect(ctx, stream)
	usage := stream.Usage()
	if usage.TotalTokens != 8 {
		t.Errorf("expected total_tokens=8, got %d", usage.TotalTokens)
	}
}

func TestCompatGenerateNilRequest(t *testing.T) {
	t.Parallel()
	p := compat.New("http://localhost:1", "")
	_, err := p.Generate(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "nil request") {
		t.Fatalf("expected nil request error, got %v", err)
	}
}

func TestCompatGenerateExperimentalReasoningUnsupported(t *testing.T) {
	t.Parallel()
	p := compat.New("http://localhost:1", "")
	_, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Options:  niro.Options{ExperimentalReasoning: true},
	})
	if err == nil || !strings.Contains(err.Error(), "experimental reasoning") {
		t.Fatalf("expected experimental reasoning error, got %v", err)
	}
}

func TestCompatConcurrentRequests(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"resp1","model":"m","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	llm := compat.New(srv.URL, "test-key")
	ctx := context.Background()

	var wg sync.WaitGroup
	errCh := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := llm.Generate(ctx, &niro.Request{
				Messages: []niro.Message{niro.UserText("hi")},
			})
			if err != nil {
				errCh <- err
				return
			}
			text, err := niro.CollectText(ctx, stream)
			if err != nil {
				errCh <- err
				return
			}
			if text != "ok" {
				errCh <- fmt.Errorf("unexpected text %q", text)
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestCompatProviderRejectsHugeToolIndex guards against an OOM where a
// malformed (or hostile) upstream stream claims a very large tool_call
// index, causing the per-chunk accumulator slice to be grown to billions
// of entries before any tool data is actually written.
func TestCompatProviderRejectsHugeToolIndex(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// Claim an outrageous tool_call index — > 1e9.
		fmt.Fprintf(w, "data: %s\n\n",
			`{"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":2000000000,"id":"call_1","function":{"name":"x","arguments":"{}"}}]}}]}`)
		flusher.Flush()
	}))
	defer srv.Close()

	llm := compat.New(srv.URL, "k", compat.WithModel("m"))
	stream, err := llm.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for stream.Next(context.Background()) {
		// drain
	}
	if stream.Err() == nil {
		t.Fatalf("expected error for out-of-range tool_call index")
	}
	if !strings.Contains(stream.Err().Error(), "out of range") {
		t.Fatalf("expected 'out of range' message, got %v", stream.Err())
	}
}

// TestCompatProviderErrorBodyBounded ensures that an upstream returning a
// massive non-2xx body cannot OOM the caller — only a bounded prefix is
// echoed in the returned error message.
func TestCompatProviderErrorBodyBounded(t *testing.T) {
	t.Parallel()

	const big = 2 << 20 // 2 MiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("E", big)))
	}))
	defer srv.Close()

	llm := compat.New(srv.URL, "k")
	_, err := llm.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err == nil {
		t.Fatal("expected error for non-2xx response")
	}
	// 64 KiB cap + framing overhead — definitely not 2 MiB.
	if got := len(err.Error()); got > (128 << 10) {
		t.Fatalf("error message not bounded: %d bytes", got)
	}
}
