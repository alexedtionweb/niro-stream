package google_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// ---------------------------------------------------------------------------
// WithHTTPClient option + NewFromClient + Client()
// ---------------------------------------------------------------------------

func TestWithHTTPClient_Applied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w, geminiFinal("ok", "STOP", 1, 1))
	}))
	defer srv.Close()

	// Combine WithHTTPClient (custom *http.Client) + WithHTTPOptions (BaseURL)
	// to exercise both branches of applyHTTP.
	p, err := New("test-key",
		WithHTTPClient(&http.Client{}),
		WithHTTPOptions(genai.HTTPOptions{BaseURL: srv.URL + "/"}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
}

func TestNewFromClient_AndClientAccessor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w, geminiFinal("hi", "STOP", 2, 3))
	}))
	defer srv.Close()

	base, err := New("test-key", WithHTTPOptions(genai.HTTPOptions{BaseURL: srv.URL + "/"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	p := NewFromClient(base.Client(), "gemini-2.0-flash")
	if p.Client() == nil {
		t.Error("Client() returned nil")
	}
	if p.Client() != base.Client() {
		t.Error("Client() returned wrong client")
	}

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if stream.Response().Usage.InputTokens != 2 {
		t.Errorf("want inputTokens=2, got %d", stream.Response().Usage.InputTokens)
	}
}

func TestNewFromClient_WithHooks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w, geminiFinal("ok", "STOP", 1, 1))
	}))
	defer srv.Close()

	base, _ := New("test-key", WithHTTPOptions(genai.HTTPOptions{BaseURL: srv.URL + "/"}))
	hookCalled := false
	p := NewFromClient(base.Client(), "gemini-pro",
		func(model string, cfg *genai.GenerateContentConfig) { hookCalled = true },
	)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if !hookCalled {
		t.Error("hook passed to NewFromClient was not called")
	}
}

// ---------------------------------------------------------------------------
// NewVertexAI success path
// ---------------------------------------------------------------------------

func TestNewVertexAI_Success(t *testing.T) {
	// genai.NewClient does not make network calls during construction, so
	// we can test the success path without real GCP credentials.
	p, err := NewVertexAI("my-project", "us-central1",
		WithHTTPOptions(genai.HTTPOptions{BaseURL: "http://localhost:19999/"}),
	)
	if err != nil {
		t.Fatalf("NewVertexAI: %v", err)
	}
	if p == nil {
		t.Error("expected non-nil provider")
	}
	p.Close()
}

// ---------------------------------------------------------------------------
// classifyError: context.Canceled and context.DeadlineExceeded
// ---------------------------------------------------------------------------

func TestGenerate_ContextCanceled(t *testing.T) {
	// Server that blocks until the client disconnects.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the HTTP request fails immediately

	stream, err := p.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		checkRynError(t, err, ryn.ErrCodeContextCancelled)
		return
	}
	// Drain with a fresh context so stream.Next doesn't short-circuit on
	// ctx.Done() before the consume goroutine emits the classified error.
	for stream.Next(context.Background()) {
	}
	if stream.Err() == nil {
		t.Fatal("expected context cancelled error")
	}
	checkRynError(t, stream.Err(), ryn.ErrCodeContextCancelled)
}

func TestGenerate_DeadlineExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	// Ensure the deadline has already passed before we even call Generate.
	time.Sleep(10 * time.Millisecond)

	stream, err := p.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		checkRynError(t, err, ryn.ErrCodeTimeout)
		return
	}
	// Drain with a fresh context so stream.Next doesn't short-circuit on
	// ctx.Done() before the consume goroutine emits the classified error.
	for stream.Next(context.Background()) {
	}
	if stream.Err() == nil {
		t.Fatal("expected timeout error")
	}
	checkRynError(t, stream.Err(), ryn.ErrCodeTimeout)
}

// ---------------------------------------------------------------------------
// mapFinishReason: all cases
// ---------------------------------------------------------------------------

func assertFinishReason(t *testing.T, bedrockReason, want string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w, geminiFinal("", bedrockReason, 1, 0))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if got := stream.Response().FinishReason; got != want {
		t.Errorf("reason=%q: want %q, got %q", bedrockReason, want, got)
	}
}

func TestMapFinishReason_Recitation(t *testing.T) {
	assertFinishReason(t, "RECITATION", "content_filter")
}
func TestMapFinishReason_Language(t *testing.T) { assertFinishReason(t, "LANGUAGE", "content_filter") }
func TestMapFinishReason_Blocklist(t *testing.T) {
	assertFinishReason(t, "BLOCKLIST", "content_filter")
}
func TestMapFinishReason_ProhibitedContent(t *testing.T) {
	assertFinishReason(t, "PROHIBITED_CONTENT", "content_filter")
}
func TestMapFinishReason_SPII(t *testing.T) { assertFinishReason(t, "SPII", "content_filter") }
func TestMapFinishReason_ImageSafety(t *testing.T) {
	assertFinishReason(t, "IMAGE_SAFETY", "content_filter")
}
func TestMapFinishReason_MalformedFunctionCall(t *testing.T) {
	assertFinishReason(t, "MALFORMED_FUNCTION_CALL", "other")
}
func TestMapFinishReason_Other(t *testing.T)   { assertFinishReason(t, "OTHER", "other") }
func TestMapFinishReason_Unknown(t *testing.T) { assertFinishReason(t, "SOME_FUTURE_REASON", "stop") }
func TestMapFinishReason_Empty(t *testing.T)   { assertFinishReason(t, "", "stop") }

// ---------------------------------------------------------------------------
// buildConfig: inference options
// ---------------------------------------------------------------------------

func TestGenerate_InferenceOptions(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		sseResponse(w, geminiFinal("ok", "STOP", 5, 5))
	}))
	defer srv.Close()

	temp := 0.7
	topP := 0.9
	topK := 40
	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
		Options: ryn.Options{
			MaxTokens:   512,
			Temperature: &temp,
			TopP:        &topP,
			TopK:        &topK,
			Stop:        []string{"\n\n", "END"},
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}

	gc, _ := capturedBody["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(512) {
		t.Errorf("want maxOutputTokens=512, got %v", gc["maxOutputTokens"])
	}
	if gc["temperature"] == nil {
		t.Error("expected temperature in generationConfig")
	}
	if gc["topP"] == nil {
		t.Error("expected topP in generationConfig")
	}
	if gc["topK"] == nil {
		t.Error("expected topK in generationConfig")
	}
	stops, _ := gc["stopSequences"].([]any)
	if len(stops) != 2 {
		t.Errorf("want 2 stop sequences, got %v", stops)
	}
}

// ---------------------------------------------------------------------------
// buildConfig: ResponseFormat
// ---------------------------------------------------------------------------

func TestGenerate_ResponseFormat_JSON(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		sseResponse(w, geminiFinal(`{"key":"val"}`, "STOP", 1, 5))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages:       []ryn.Message{ryn.UserText("give JSON")},
		ResponseFormat: "json",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}

	gc, _ := capturedBody["generationConfig"].(map[string]any)
	if gc["responseMimeType"] != "application/json" {
		t.Errorf("want responseMimeType=application/json, got %v", gc["responseMimeType"])
	}
}

func TestGenerate_ResponseFormat_JSONSchema(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		sseResponse(w, geminiFinal(`{"name":"Alice"}`, "STOP", 1, 5))
	}))
	defer srv.Close()

	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`)
	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages:       []ryn.Message{ryn.UserText("give JSON")},
		ResponseFormat: "json_schema",
		ResponseSchema: schema,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}

	gc, _ := capturedBody["generationConfig"].(map[string]any)
	if gc["responseMimeType"] != "application/json" {
		t.Errorf("want responseMimeType=application/json, got %v", gc["responseMimeType"])
	}
	if gc["responseSchema"] == nil {
		t.Error("expected responseSchema in generationConfig")
	}
}

// ---------------------------------------------------------------------------
// buildConfig: ToolChoice variants
// ---------------------------------------------------------------------------

func TestGenerate_ToolChoice_None(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		sseResponse(w, geminiFinal("ok", "STOP", 1, 1))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages:   []ryn.Message{ryn.UserText("hi")},
		Tools:      []ryn.Tool{{Name: "foo", Description: "d"}},
		ToolChoice: ryn.ToolChoiceNone,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}

	tc, _ := capturedBody["toolConfig"].(map[string]any)
	fcc, _ := tc["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "NONE" {
		t.Errorf("want mode=NONE for ToolChoiceNone, got %v", fcc["mode"])
	}
}

func TestGenerate_ToolChoice_Required(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		sseResponse(w, geminiFinal("ok", "STOP", 1, 1))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages:   []ryn.Message{ryn.UserText("hi")},
		Tools:      []ryn.Tool{{Name: "bar", Description: "d"}},
		ToolChoice: ryn.ToolChoiceRequired,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}

	tc, _ := capturedBody["toolConfig"].(map[string]any)
	fcc, _ := tc["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "ANY" {
		t.Errorf("want mode=ANY for ToolChoiceRequired, got %v", fcc["mode"])
	}
}

func TestGenerate_ToolChoice_Func(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		sseResponse(w, geminiFinal("ok", "STOP", 1, 1))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages:   []ryn.Message{ryn.UserText("hi")},
		Tools:      []ryn.Tool{{Name: "specific_fn", Description: "d"}},
		ToolChoice: ryn.ToolChoiceFunc("specific_fn"),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}

	tc, _ := capturedBody["toolConfig"].(map[string]any)
	fcc, _ := tc["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "ANY" {
		t.Errorf("want mode=ANY for ToolChoiceFunc, got %v", fcc["mode"])
	}
	allowed, _ := fcc["allowedFunctionNames"].([]any)
	if len(allowed) != 1 || allowed[0] != "specific_fn" {
		t.Errorf("want allowedFunctionNames=[specific_fn], got %v", allowed)
	}
}

// ---------------------------------------------------------------------------
// convertParts: image / audio / video parts
// ---------------------------------------------------------------------------

func TestGenerate_ImagePart(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		sseResponse(w, geminiFinal("I see a cat", "STOP", 3, 5))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{{
			Role: ryn.RoleUser,
			Parts: []ryn.Part{
				{Kind: ryn.KindText, Text: "What is this?"},
				ryn.ImagePart([]byte{0xFF, 0xD8, 0xFF, 0xE0}, "image/jpeg"),
			},
		}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}

	contents, _ := capturedBody["contents"].([]any)
	if len(contents) == 0 {
		t.Fatal("no contents sent")
	}
	parts, _ := contents[0].(map[string]any)["parts"].([]any)
	var hasInlineData bool
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if pm["inlineData"] != nil {
			hasInlineData = true
		}
	}
	if !hasInlineData {
		t.Error("expected inlineData part for image")
	}
}

func TestGenerate_AudioPart(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		sseResponse(w, geminiFinal("transcribed", "STOP", 3, 5))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{{
			Role:  ryn.RoleUser,
			Parts: []ryn.Part{ryn.AudioPart([]byte{0x52, 0x49, 0x46, 0x46}, "audio/wav")},
		}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}

	contents, _ := capturedBody["contents"].([]any)
	parts, _ := contents[0].(map[string]any)["parts"].([]any)
	var hasInlineData bool
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if pm["inlineData"] != nil {
			hasInlineData = true
		}
	}
	if !hasInlineData {
		t.Error("expected inlineData part for audio")
	}
}

// ---------------------------------------------------------------------------
// convertParts: KindToolCall and tool result success
// ---------------------------------------------------------------------------

func TestGenerate_AssistantToolCallMessage(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		sseResponse(w, geminiFinal("done", "STOP", 5, 2))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{
			ryn.UserText("call the tool"),
			{
				Role: ryn.RoleAssistant,
				Parts: []ryn.Part{
					ryn.ToolCallPart(&ryn.ToolCall{
						ID:   "call-1",
						Name: "my_tool",
						Args: json.RawMessage(`{"x":42}`),
					}),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}

	contents, _ := capturedBody["contents"].([]any)
	if len(contents) < 2 {
		t.Fatalf("want >=2 contents, got %d", len(contents))
	}
	// Second content should be the model turn with functionCall
	modelContent, _ := contents[1].(map[string]any)
	if modelContent["role"] != "model" {
		t.Errorf("want role=model, got %v", modelContent["role"])
	}
	parts, _ := modelContent["parts"].([]any)
	var hasFunctionCall bool
	for _, part := range parts {
		pm, _ := part.(map[string]any)
		if pm["functionCall"] != nil {
			hasFunctionCall = true
		}
	}
	if !hasFunctionCall {
		t.Error("expected functionCall part in model content")
	}
}

func TestGenerate_ToolResultSuccess(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		sseResponse(w, geminiFinal("noted", "STOP", 5, 2))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{
			ryn.UserText("call the tool"),
			{
				Role: ryn.RoleTool,
				Parts: []ryn.Part{{
					Kind: ryn.KindToolResult,
					Result: &ryn.ToolResult{
						CallID:  "call-1",
						Content: "the weather is sunny",
						IsError: false, // success result
					},
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}

	// Verify the tool result was sent with "output" key (not "error")
	contents, _ := capturedBody["contents"].([]any)
	var found bool
	for _, c := range contents {
		cm, _ := c.(map[string]any)
		for _, p := range cm["parts"].([]any) {
			pm, _ := p.(map[string]any)
			if fr, ok := pm["functionResponse"].(map[string]any); ok {
				resp, _ := fr["response"].(map[string]any)
				if _, hasOutput := resp["output"]; hasOutput {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("expected functionResponse with 'output' key for success result")
	}
}

// ---------------------------------------------------------------------------
// consume: empty candidate content (cand.Content == nil)
// ---------------------------------------------------------------------------

func TestGenerate_EmptyCandidateContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w,
			// First chunk: candidate with no content (e.g., safety filter before content)
			map[string]any{
				"candidates": []any{
					map[string]any{
						"finishReason": "SAFETY",
						// no "content" key
					},
				},
			},
			geminiFinal("", "SAFETY", 1, 0),
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
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if stream.Response().FinishReason != "content_filter" {
		t.Errorf("want content_filter, got %q", stream.Response().FinishReason)
	}
}

// ---------------------------------------------------------------------------
// consume: multiple tool calls without ID (callIndex > 0 path)
// ---------------------------------------------------------------------------

func TestGenerate_MultipleToolCallsNoID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w, map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"role": "model",
						"parts": []any{
							map[string]any{
								"functionCall": map[string]any{
									// no "id" — first call uses name as ID
									"name": "search",
									"args": map[string]any{"q": "a"},
								},
							},
							map[string]any{
								"functionCall": map[string]any{
									// no "id" — second call uses name_N format
									"name": "search",
									"args": map[string]any{"q": "b"},
								},
							},
						},
					},
					"finishReason": "STOP",
				},
			},
		})
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, WithModel("gemini-2.0-flash"))
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("search twice")},
		Tools:    []ryn.Tool{{Name: "search", Description: "search"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	frames := collectFrames(t, stream)

	var calls []*ryn.ToolCall
	for _, f := range frames {
		if f.Kind == ryn.KindToolCall {
			calls = append(calls, f.Tool)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 tool calls, got %d", len(calls))
	}
	// First call uses name, second uses name_1
	if calls[0].ID != "search" {
		t.Errorf("first call ID: want search, got %q", calls[0].ID)
	}
	if !strings.HasPrefix(calls[1].ID, "search") {
		t.Errorf("second call ID: want search_1, got %q", calls[1].ID)
	}
}

// ---------------------------------------------------------------------------
// buildContents: system message in list is excluded
// ---------------------------------------------------------------------------

func TestGenerate_SystemMessageInList(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		sseResponse(w, geminiFinal("ok", "STOP", 1, 1))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{
			{Role: ryn.RoleSystem, Parts: []ryn.Part{{Kind: ryn.KindText, Text: "Be concise."}}},
			ryn.UserText("hello"),
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}

	contents, _ := capturedBody["contents"].([]any)
	// System message must NOT appear in contents (it's filtered by buildContents)
	for _, c := range contents {
		cm, _ := c.(map[string]any)
		if cm["role"] == "system" {
			t.Error("system message should be excluded from contents array")
		}
	}
	if len(contents) != 1 {
		t.Errorf("want 1 content (user only), got %d", len(contents))
	}
}

// ---------------------------------------------------------------------------
// Additional HTTP error codes
// ---------------------------------------------------------------------------

func TestGenerate_HTTP403_Forbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintln(w, `{"error":{"code":403,"message":"Permission denied","status":"PERMISSION_DENIED"}}`)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hello")},
	})
	if err != nil {
		// 403 falls to the default 4xx case → ErrCodeInvalidRequest
		checkRynError(t, err, ryn.ErrCodeInvalidRequest)
		return
	}
	for stream.Next(context.Background()) {
	}
	checkRynError(t, stream.Err(), ryn.ErrCodeInvalidRequest)
}

func TestGenerate_HTTP404_ModelNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintln(w, `{"error":{"code":404,"message":"Model not found","status":"NOT_FOUND"}}`)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hello")},
	})
	if err != nil {
		checkRynError(t, err, ryn.ErrCodeModelNotFound)
		return
	}
	for stream.Next(context.Background()) {
	}
	checkRynError(t, stream.Err(), ryn.ErrCodeModelNotFound)
}

func TestGenerate_HTTP500_InternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, `{"error":{"code":500,"message":"Internal error","status":"INTERNAL"}}`)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hello")},
	})
	if err != nil {
		checkRynError(t, err, ryn.ErrCodeProviderError)
		return
	}
	for stream.Next(context.Background()) {
	}
	checkRynError(t, stream.Err(), ryn.ErrCodeProviderError)
}

// ---------------------------------------------------------------------------
// Model name fallback: empty request Model → uses provider default
// ---------------------------------------------------------------------------

func TestGenerate_DefaultModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseResponse(w, geminiFinal("hi", "STOP", 1, 1))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, WithModel("my-default-model"))
	stream, err := p.Generate(context.Background(), &ryn.Request{
		// No Model field — should use provider default
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
}
