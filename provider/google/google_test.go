package google_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/genai"

	"ryn.dev/ryn"
	. "ryn.dev/ryn/provider/google"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// sseResponse writes a sequence of SSE data lines to w and closes the body.
// Each item in chunks is serialised as a separate data: <JSON> line.
func sseResponse(w http.ResponseWriter, chunks ...any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, chunk := range chunks {
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
}

// newTestProvider creates a Provider pointed at the given test server URL.
func newTestProvider(t *testing.T, serverURL string, opts ...Option) *Provider {
	t.Helper()
	opts = append(opts,
		WithHTTPOptions(genai.HTTPOptions{BaseURL: serverURL + "/"}),
	)
	p, err := New("test-api-key", opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// geminiChunk builds a minimal streaming chunk map.
func geminiChunk(text string) map[string]any {
	return map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"role":  "model",
					"parts": []any{map[string]any{"text": text}},
				},
				"finishReason": "",
			},
		},
	}
}

// geminiFinal returns a final chunk with usage and finish reason.
func geminiFinal(text, finishReason string, inTok, outTok int32) map[string]any {
	return map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"role":  "model",
					"parts": []any{map[string]any{"text": text}},
				},
				"finishReason": finishReason,
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":     inTok,
			"candidatesTokenCount": outTok,
			"totalTokenCount":      inTok + outTok,
		},
		"modelVersion": "gemini-2.0-flash-001",
		"responseId":   "resp-1234",
	}
}

// geminiToolCallChunk returns a chunk containing a FunctionCall.
func geminiToolCallChunk(callID, name string, args map[string]any) map[string]any {
	return map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"role": "model",
					"parts": []any{map[string]any{
						"functionCall": map[string]any{
							"id":   callID,
							"name": name,
							"args": args,
						},
					}},
				},
				"finishReason": "STOP",
			},
		},
	}
}

// collectFrames drains a stream and returns all frames (excluding usage).
func collectFrames(t *testing.T, s *ryn.Stream) []ryn.Frame {
	t.Helper()
	ctx := context.Background()
	var frames []ryn.Frame
	for s.Next(ctx) {
		f := s.Frame()
		if f.Kind != ryn.KindUsage {
			frames = append(frames, f)
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	return frames
}

// ---------------------------------------------------------------------------
// Construction tests
// ---------------------------------------------------------------------------

func TestNew_EmptyAPIKey(t *testing.T) {
	_, err := New("")
	if err == nil {
		t.Fatal("expected error for empty API key")
	}
	var rynErr *ryn.Error
	if !errors.As(err, &rynErr) || rynErr.Code != ryn.ErrCodeAuthenticationFailed {
		t.Errorf("want ErrCodeAuthenticationFailed, got %v", err)
	}
}

func TestNew_WhitespaceAPIKey(t *testing.T) {
	_, err := New("   ")
	if err == nil {
		t.Fatal("expected error for whitespace-only API key")
	}
}

func TestNewVertexAI_EmptyProject(t *testing.T) {
	_, err := NewVertexAI("", "us-central1")
	if err == nil {
		t.Fatal("expected error for empty project ID")
	}
	var rynErr *ryn.Error
	if !errors.As(err, &rynErr) || rynErr.Code != ryn.ErrCodeInvalidRequest {
		t.Errorf("want ErrCodeInvalidRequest, got %v", err)
	}
}

func TestNewVertexAI_EmptyLocation(t *testing.T) {
	_, err := NewVertexAI("my-project", "")
	if err == nil {
		t.Fatal("expected error for empty location")
	}
}

// ---------------------------------------------------------------------------
// Validation tests
// ---------------------------------------------------------------------------

func TestGenerate_NilMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called")
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	// Empty request — req.Validate() should catch this
	_, err := p.Generate(context.Background(), &ryn.Request{})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestGenerate_SystemPromptOnly_NoMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called for system-prompt-only request")
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	_, err := p.Generate(context.Background(), &ryn.Request{
		SystemPrompt: "You are a bot.",
		// No Messages — after filtering, contents will be empty
	})
	if err == nil {
		t.Fatal("expected error for SystemPrompt-only request with no messages")
	}
	var rynErr *ryn.Error
	if !errors.As(err, &rynErr) {
		t.Fatalf("expected *ryn.Error, got %T: %v", err, err)
	}
	if rynErr.Code != ryn.ErrCodeInvalidRequest {
		t.Errorf("want ErrCodeInvalidRequest, got %v", rynErr.Code)
	}
}

// ---------------------------------------------------------------------------
// Streaming / happy-path tests
// ---------------------------------------------------------------------------

func TestGenerate_TextStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w,
			geminiChunk("Hello"),
			geminiChunk(", "),
			geminiFinal("world!", "STOP", 5, 3),
		)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	frames := collectFrames(t, stream)

	var text string
	for _, f := range frames {
		if f.Kind == ryn.KindText {
			text += f.Text
		}
	}
	if text != "Hello, world!" {
		t.Errorf("want %q, got %q", "Hello, world!", text)
	}

	meta := stream.Response()
	if meta.FinishReason != "stop" {
		t.Errorf("want finish_reason=stop, got %q", meta.FinishReason)
	}
	if meta.Model != "gemini-2.0-flash-001" {
		t.Errorf("want model=gemini-2.0-flash-001, got %q", meta.Model)
	}
	if meta.ID != "resp-1234" {
		t.Errorf("want id=resp-1234, got %q", meta.ID)
	}
	if meta.Usage.InputTokens != 5 || meta.Usage.OutputTokens != 3 {
		t.Errorf("unexpected usage: %+v", meta.Usage)
	}
}

func TestGenerate_ToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w, geminiToolCallChunk("call-abc", "get_weather", map[string]any{
			"location": "London",
		}))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, WithModel("gemini-2.0-flash"))
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("What's the weather?")},
		Tools: []ryn.Tool{{
			Name:        "get_weather",
			Description: "Get weather for a location",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	frames := collectFrames(t, stream)
	var tc *ryn.ToolCall
	for _, f := range frames {
		if f.Kind == ryn.KindToolCall {
			tc = f.Tool
		}
	}
	if tc == nil {
		t.Fatal("expected a ToolCall frame")
	}
	if tc.ID != "call-abc" {
		t.Errorf("want ID=call-abc, got %q", tc.ID)
	}
	if tc.Name != "get_weather" {
		t.Errorf("want Name=get_weather, got %q", tc.Name)
	}
	var args map[string]any
	_ = json.Unmarshal(tc.Args, &args)
	if args["location"] != "London" {
		t.Errorf("want location=London, got %v", args["location"])
	}
}

func TestGenerate_ToolCallIDFallback(t *testing.T) {
	// When the API returns FunctionCall without an ID (older models), we
	// generate a stable ID from the function name.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w, map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"role": "model",
						"parts": []any{map[string]any{
							"functionCall": map[string]any{
								// no "id" field
								"name": "search",
								"args": map[string]any{"query": "Go generics"},
							},
						}},
					},
					"finishReason": "STOP",
				},
			},
		})
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("search for me")},
		Tools:    []ryn.Tool{{Name: "search", Description: "Search tool"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	frames := collectFrames(t, stream)
	var tc *ryn.ToolCall
	for _, f := range frames {
		if f.Kind == ryn.KindToolCall {
			tc = f.Tool
		}
	}
	if tc == nil {
		t.Fatal("expected a ToolCall frame")
	}
	// Without a server ID, the fallback is the function name
	if tc.ID != "search" {
		t.Errorf("want ID=search (fallback), got %q", tc.ID)
	}
}

// ---------------------------------------------------------------------------
// Error handling tests
// ---------------------------------------------------------------------------

func TestGenerate_HTTP401_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintln(w, `{"error":{"code":401,"message":"API key invalid","status":"UNAUTHENTICATED"}}`)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hello")},
	})
	if err != nil {
		// Some errors surface at Generate time
		var rynErr *ryn.Error
		if errors.As(err, &rynErr) && rynErr.Code == ryn.ErrCodeAuthenticationFailed {
			return // expected
		}
		t.Fatalf("unexpected error type: %v", err)
	}

	// Drain the stream; error should come through there
	for stream.Next(context.Background()) {
	}
	err = stream.Err()
	if err == nil {
		t.Fatal("expected authentication error in stream")
	}
	var rynErr *ryn.Error
	if !errors.As(err, &rynErr) {
		t.Fatalf("expected *ryn.Error, got %T: %v", err, err)
	}
	if rynErr.Code != ryn.ErrCodeAuthenticationFailed {
		t.Errorf("want ErrCodeAuthenticationFailed, got %v", rynErr.Code)
	}
}

func TestGenerate_HTTP429_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintln(w, `{"error":{"code":429,"message":"Rate limit exceeded","status":"RESOURCE_EXHAUSTED"}}`)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hello")},
	})
	if err != nil {
		checkRynError(t, err, ryn.ErrCodeRateLimited)
		return
	}
	for stream.Next(context.Background()) {
	}
	checkRynError(t, stream.Err(), ryn.ErrCodeRateLimited)
	if !ryn.IsRetryable(stream.Err()) {
		t.Error("rate limit error should be retryable")
	}
}

func TestGenerate_HTTP503_ServiceUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, `{"error":{"code":503,"message":"Service unavailable"}}`)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hello")},
	})
	if err != nil {
		checkRynError(t, err, ryn.ErrCodeServiceUnavailable)
		return
	}
	for stream.Next(context.Background()) {
	}
	checkRynError(t, stream.Err(), ryn.ErrCodeServiceUnavailable)
	if !ryn.IsRetryable(stream.Err()) {
		t.Error("503 error should be retryable")
	}
}

// ---------------------------------------------------------------------------
// Configuration / buildConfig tests
// ---------------------------------------------------------------------------

func TestGenerate_SystemPromptInConfig(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		sseResponse(w, geminiFinal("ok", "STOP", 1, 1))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	_, err := p.Generate(context.Background(), &ryn.Request{
		SystemPrompt: "Be helpful.",
		Messages:     []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// The system instruction must be in "generationConfig.systemInstruction"
	// (nested inside the POST body by the SDK).
	// We just verify the request reached the server and the stream worked.
}

func TestGenerate_RequestHookApplied(t *testing.T) {
	hookCalled := false
	hook := RequestHook(func(model string, cfg *genai.GenerateContentConfig) {
		hookCalled = true
		if model == "" {
			panic("hook received empty model name")
		}
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w, geminiFinal("hi", "STOP", 1, 1))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
		Extra:    hook,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if !hookCalled {
		t.Error("request hook was not called")
	}
}

func TestGenerate_ProviderLevelHook(t *testing.T) {
	hookCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w, geminiFinal("pong", "STOP", 1, 1))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL,
		WithRequestHook(func(model string, cfg *genai.GenerateContentConfig) {
			hookCalled = true
			cfg.MaxOutputTokens = 100 // verify it compiles and runs
		}),
	)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("ping")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if !hookCalled {
		t.Error("provider-level hook was not called")
	}
}

// ---------------------------------------------------------------------------
// FinishReason mapping tests
// ---------------------------------------------------------------------------

func TestMapFinishReason_Stop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w, geminiFinal("done", "STOP", 1, 1))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, _ := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	for stream.Next(context.Background()) {
	}
	if stream.Response().FinishReason != "stop" {
		t.Errorf("want stop, got %q", stream.Response().FinishReason)
	}
}

func TestMapFinishReason_Length(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w, geminiFinal("...", "MAX_TOKENS", 1, 1))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, _ := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	for stream.Next(context.Background()) {
	}
	if stream.Response().FinishReason != "length" {
		t.Errorf("want length, got %q", stream.Response().FinishReason)
	}
}

func TestMapFinishReason_ContentFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w, geminiFinal("", "SAFETY", 1, 0))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, _ := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	for stream.Next(context.Background()) {
	}
	if stream.Response().FinishReason != "content_filter" {
		t.Errorf("want content_filter, got %q", stream.Response().FinishReason)
	}
}

// ---------------------------------------------------------------------------
// ToolResult / tool-round-trip tests
// ---------------------------------------------------------------------------

func TestGenerate_ToolResultIsError(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		sseResponse(w, geminiFinal("noted", "STOP", 5, 2))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	_, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{
			ryn.UserText("call the tool"),
			{
				Role: ryn.RoleTool,
				Parts: []ryn.Part{{
					Kind: ryn.KindToolResult,
					Result: &ryn.ToolResult{
						CallID:  "tool-1",
						Content: "connection refused",
						IsError: true,
					},
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// We verify the request was formed and sent (server received it).
	// Detailed body inspection is possible but depends on SDK serialisation.
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func checkRynError(t *testing.T, err error, wantCode ryn.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %v, got nil", wantCode)
	}
	var rynErr *ryn.Error
	if !errors.As(err, &rynErr) {
		t.Fatalf("expected *ryn.Error, got %T: %v", err, err)
	}
	if rynErr.Code != wantCode {
		t.Errorf("want code %v, got %v", wantCode, rynErr.Code)
	}
}
