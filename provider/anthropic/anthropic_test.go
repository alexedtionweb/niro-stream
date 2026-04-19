package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	ant "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/alexedtionweb/niro-stream"
	. "github.com/alexedtionweb/niro-stream/provider/anthropic"
)

// ─── SSE helpers ─────────────────────────────────────────────────────────────

// sseEvent writes a single SSE event in Anthropic format:
// "event: <type>\ndata: <json>\n\n"
func sseEvent(eventType string, data any) string {
	b, _ := json.Marshal(data)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, b)
}

// pingEvent returns a ping SSE event (SDK should skip it).
func pingEvent() string {
	return "event: ping\ndata: {\"type\":\"ping\"}\n\n"
}

// A minimal Anthropic SSE streaming session:
//
//	message_start → content_block_start (text) → content_block_delta(s) →
//	content_block_stop → message_delta(stop_reason) → message_stop
func sseTextStream(id, model, text string) string {
	var b strings.Builder
	// message_start
	b.WriteString(sseEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":          id,
			"type":        "message",
			"role":        "assistant",
			"model":       model,
			"content":     []any{},
			"stop_reason": nil,
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 1,
			},
		},
	}))
	// content_block_start (text)
	b.WriteString(sseEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	}))
	// content_block_delta
	b.WriteString(sseEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{
			"type": "text_delta",
			"text": text,
		},
	}))
	// content_block_stop
	b.WriteString(sseEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	}))
	// message_delta (stop reason + output token count)
	b.WriteString(sseEvent("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": 5,
		},
	}))
	// message_stop
	b.WriteString(sseEvent("message_stop", map[string]any{
		"type": "message_stop",
	}))
	return b.String()
}

// sseToolStream produces a streaming session that returns a single tool call.
func sseToolStream(id, model, callID, funcName, args string) string {
	var b strings.Builder
	b.WriteString(sseEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":          id,
			"type":        "message",
			"role":        "assistant",
			"model":       model,
			"content":     []any{},
			"stop_reason": nil,
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 1,
			},
		},
	}))
	// tool_use block start
	b.WriteString(sseEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    callID,
			"name":  funcName,
			"input": map[string]any{},
		},
	}))
	// input_json_delta chunks
	b.WriteString(sseEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": args,
		},
	}))
	// content_block_stop
	b.WriteString(sseEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	}))
	// message_delta
	b.WriteString(sseEvent("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   "tool_use",
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": 15,
		},
	}))
	// message_stop
	b.WriteString(sseEvent("message_stop", map[string]any{
		"type": "message_stop",
	}))
	return b.String()
}

// errBody returns a JSON error body in Anthropic's format.
func errBody(errType, msg string) string {
	return fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, errType, msg)
}

// ─── Mock helpers ─────────────────────────────────────────────────────────────

// mockMiddleware returns a request option that intercepts all HTTP requests.
func mockMiddleware(handler func(*http.Request) (int, string, string)) option.RequestOption {
	return option.WithMiddleware(func(req *http.Request, _ option.MiddlewareNext) (*http.Response, error) {
		code, body, ct := handler(req)
		if ct == "" {
			ct = "text/event-stream"
		}
		return &http.Response{
			StatusCode: code,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
}

// newProvider builds a Provider wired to the given mock, with retries disabled.
func newProvider(t *testing.T, handler func(*http.Request) (int, string, string), opts ...Option) *Provider {
	t.Helper()
	mw := mockMiddleware(handler)
	noRetry := WithRequestOption(option.WithMaxRetries(0))
	allOpts := append([]Option{WithRequestOption(mw), noRetry}, opts...)
	return New("test-key", allOpts...)
}

// collectText drains a stream and returns the concatenated text + any error.
func collectText(t *testing.T, s *niro.Stream) (string, error) {
	t.Helper()
	return niro.CollectText(context.Background(), s)
}

// captureRequest decodes the JSON body sent to the mock server.
func captureRequest(t *testing.T, req *niro.Request, opts ...Option) map[string]any {
	t.Helper()
	var captured map[string]any
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		return 200, sseTextStream("id1", "claude-sonnet-4-5", "ok"), ""
	}, opts...)
	stream, err := p.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	return captured
}

// ─── Constructor and accessors ────────────────────────────────────────────────

func TestNew_DefaultModel(t *testing.T) {
	var gotModel string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		return 200, sseTextStream("id1", "claude-sonnet-4-5", "hi"), ""
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("ping")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	text, err := collectText(t, stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if text != "hi" {
		t.Errorf("text = %q, want %q", text, "hi")
	}
	if gotModel != string(ant.ModelClaudeSonnet4_5) {
		t.Errorf("model = %q, want default claude-sonnet-4-5", gotModel)
	}
}

func TestNew_CustomModel(t *testing.T) {
	var gotModel string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		return 200, sseTextStream("id1", "claude-3-5-haiku-latest", "ok"), ""
	}, WithModel("claude-3-5-haiku-latest"))
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if gotModel != "claude-3-5-haiku-latest" {
		t.Errorf("model = %q, want claude-3-5-haiku-latest", gotModel)
	}
}

func TestNew_RequestModelOverridesDefault(t *testing.T) {
	var gotModel string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		return 200, sseTextStream("id1", "claude-3-opus-latest", "ok"), ""
	}, WithModel("claude-sonnet-4-5"))
	stream, err := p.Generate(context.Background(), &niro.Request{
		Model:    "claude-3-opus-latest",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if gotModel != "claude-3-opus-latest" {
		t.Errorf("model = %q, want claude-3-opus-latest", gotModel)
	}
}

func TestNewFromClient_AndClient(t *testing.T) {
	underlying := ant.NewClient(option.WithAPIKey("k"))
	p := NewFromClient(underlying, "claude-sonnet-4-5")
	if p == nil {
		t.Fatal("NewFromClient returned nil")
	}
	_ = p.Client() // must compile and not panic
}

// ─── Request option ───────────────────────────────────────────────────────────

func TestWithRequestOption(t *testing.T) {
	// WithRequestOption is already used internally; verify additional headers land.
	var gotHeader string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		gotHeader = r.Header.Get("X-Custom")
		return 200, sseTextStream("id1", "claude-sonnet-4-5", "ok"), ""
	}, WithRequestOption(option.WithHeader("X-Custom", "test-value")))
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if gotHeader != "test-value" {
		t.Errorf("X-Custom header = %q, want test-value", gotHeader)
	}
}

// ─── Hooks ────────────────────────────────────────────────────────────────────

func TestWithRequestHook_ProviderLevel(t *testing.T) {
	hookCalled := false
	var gotMetaUser string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if meta, ok := body["metadata"].(map[string]any); ok {
			gotMetaUser, _ = meta["user_id"].(string)
		}
		return 200, sseTextStream("id1", "claude-sonnet-4-5", "ok"), ""
	}, WithRequestHook(func(p *ant.MessageNewParams) {
		hookCalled = true
		p.Metadata = ant.MetadataParam{
			UserID: ant.String("u-001"),
		}
	}))
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if !hookCalled {
		t.Error("provider-level hook not called")
	}
	if gotMetaUser != "u-001" {
		t.Errorf("metadata.user_id = %q, want u-001", gotMetaUser)
	}
}

func TestWithRequestHook_MultipleHooks(t *testing.T) {
	var calls []string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, sseTextStream("id1", "claude-sonnet-4-5", "ok"), ""
	},
		WithRequestHook(func(p *ant.MessageNewParams) { calls = append(calls, "first") }),
		WithRequestHook(func(p *ant.MessageNewParams) { calls = append(calls, "second") }),
	)
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Errorf("hooks called in wrong order: %v", calls)
	}
}

func TestGenerate_PerRequestHook(t *testing.T) {
	var gotMetaUser string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if meta, ok := body["metadata"].(map[string]any); ok {
			gotMetaUser, _ = meta["user_id"].(string)
		}
		return 200, sseTextStream("id1", "claude-sonnet-4-5", "ok"), ""
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Extra: RequestHook(func(params *ant.MessageNewParams) {
			params.Metadata = ant.MetadataParam{
				UserID: ant.String("per-req-user"),
			}
		}),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if gotMetaUser != "per-req-user" {
		t.Errorf("metadata.user_id = %q, want per-req-user", gotMetaUser)
	}
}

// ─── Text streaming ───────────────────────────────────────────────────────────

func TestGenerate_TextStream(t *testing.T) {
	// Send two separate text delta events to test multi-chunk text assembly.
	var b strings.Builder
	b.WriteString(sseEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "id1", "type": "message", "role": "assistant",
			"model": "claude-sonnet-4-5", "content": []any{}, "stop_reason": nil,
			"usage": map[string]any{"input_tokens": 5, "output_tokens": 1},
		},
	}))
	b.WriteString(sseEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	}))
	b.WriteString(sseEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "Hello"},
	}))
	b.WriteString(sseEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": " world"},
	}))
	b.WriteString(sseEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": 0,
	}))
	b.WriteString(sseEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 2},
	}))
	b.WriteString(sseEvent("message_stop", map[string]any{"type": "message_stop"}))
	body := b.String()

	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, body, ""
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	text, err := collectText(t, stream)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if text != "Hello world" {
		t.Errorf("text = %q, want %q", text, "Hello world")
	}
}

func TestGenerate_PingEventIgnored(t *testing.T) {
	var b strings.Builder
	b.WriteString(pingEvent())
	b.WriteString(sseTextStream("id1", "claude-sonnet-4-5", "pong"))
	body := b.String()

	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, body, ""
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("ping")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	text, err := collectText(t, stream)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if text != "pong" {
		t.Errorf("text = %q, want %q", text, "pong")
	}
}

func TestGenerate_UsageAccumulated(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, sseTextStream("id1", "claude-sonnet-4-5", "hi"), ""
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	usage := stream.Usage()
	// message_start sets input_tokens=10; message_delta sets output_tokens=5
	if usage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", usage.InputTokens)
	}
	if usage.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", usage.OutputTokens)
	}
	if usage.TotalTokens != 15 {
		t.Errorf("TotalTokens = %d, want 15", usage.TotalTokens)
	}
}

func TestGenerate_ResponseMeta(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, sseTextStream("msg-abc", "claude-sonnet-4-5", "hi"), ""
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	meta := stream.Response()
	if meta == nil {
		t.Fatal("response meta is nil")
	}
	if meta.ID != "msg-abc" {
		t.Errorf("ID = %q, want msg-abc", meta.ID)
	}
	if meta.Model != "claude-sonnet-4-5" {
		t.Errorf("Model = %q, want claude-sonnet-4-5", meta.Model)
	}
	if meta.FinishReason != "end_turn" {
		t.Errorf("FinishReason = %q, want end_turn", meta.FinishReason)
	}
}

// ─── Tool calls ──────────────────────────────────────────────────────────────

func TestGenerate_ToolCall(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, sseToolStream("id1", "claude-sonnet-4-5", "call-1", "get_weather", `{"city":"Paris"}`), ""
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("weather?")},
		Tools: []niro.Tool{{
			Name:        "get_weather",
			Description: "Gets the weather",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	frames, err := niro.Collect(context.Background(), stream)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}
	var toolFrames []niro.Frame
	for _, f := range frames {
		if f.Kind == niro.KindToolCall {
			toolFrames = append(toolFrames, f)
		}
	}
	if len(toolFrames) != 1 {
		t.Fatalf("expected 1 tool call frame, got %d", len(toolFrames))
	}
	tc := toolFrames[0].Tool
	if tc.ID != "call-1" {
		t.Errorf("ID = %q, want call-1", tc.ID)
	}
	if tc.Name != "get_weather" {
		t.Errorf("Name = %q, want get_weather", tc.Name)
	}
	if string(tc.Args) != `{"city":"Paris"}` {
		t.Errorf("Args = %s, want {\"city\":\"Paris\"}", tc.Args)
	}
}

func TestGenerate_ToolCallFinishReason(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, sseToolStream("id1", "claude-sonnet-4-5", "call-x", "fn", `{}`), ""
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("use tool")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	meta := stream.Response()
	if meta == nil {
		t.Fatal("meta is nil")
	}
	if meta.FinishReason != "tool_use" {
		t.Errorf("FinishReason = %q, want tool_use", meta.FinishReason)
	}
}

// ─── Context cancellation ─────────────────────────────────────────────────────

func TestGenerate_ContextCancelled(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, sseTextStream("id1", "claude-sonnet-4-5", "hello"), ""
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	stream, err := p.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(ctx) {
	}
	err = stream.Err()
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestGenerate_ContextDeadline(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, sseTextStream("id1", "claude-sonnet-4-5", "hello"), ""
	})
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(1 * time.Millisecond)

	stream, err := p.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("want DeadlineExceeded, got %v", err)
		}
	}
}

// ─── Stream errors ────────────────────────────────────────────────────────────

func TestGenerate_StreamError_400(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 400, errBody("invalid_request_error", "bad request"), "application/json"
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	err = stream.Err()
	if err == nil {
		t.Fatal("expected stream error, got nil")
	}
	if !strings.Contains(err.Error(), "stream:") {
		t.Errorf("error should contain 'stream:', got: %v", err)
	}
}

func TestGenerate_StreamError_401(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 401, errBody("authentication_error", "invalid api key"), "application/json"
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	err = stream.Err()
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *ant.Error
	if !errors.As(err, &apiErr) {
		t.Errorf("expected *ant.Error in chain, got: %T %v", err, err)
	}
	if apiErr != nil && apiErr.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
}

func TestGenerate_StreamError_429(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 429, errBody("rate_limit_error", "rate limit exceeded"), "application/json"
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	err = stream.Err()
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *ant.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
}

func TestGenerate_StreamError_500(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 500, errBody("api_error", "internal server error"), "application/json"
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err == nil {
		t.Fatal("expected error for 500")
	}
}

// ─── buildParams: Options ────────────────────────────────────────────────────

func TestBuildParams_DefaultMaxTokens(t *testing.T) {
	body := captureRequest(t, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	mt, _ := body["max_tokens"].(float64)
	if mt != 4096 {
		t.Errorf("max_tokens = %v, want 4096 (default)", mt)
	}
}

func TestBuildParams_ExplicitMaxTokens(t *testing.T) {
	body := captureRequest(t, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Options:  niro.Options{MaxTokens: 512},
	})
	mt, _ := body["max_tokens"].(float64)
	if mt != 512 {
		t.Errorf("max_tokens = %v, want 512", mt)
	}
}

func TestBuildParams_Temperature(t *testing.T) {
	body := captureRequest(t, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Options:  niro.Options{Temperature: niro.Temp(0.8)},
	})
	temp, _ := body["temperature"].(float64)
	if temp != 0.8 {
		t.Errorf("temperature = %v, want 0.8", temp)
	}
}

func TestBuildParams_TopP(t *testing.T) {
	body := captureRequest(t, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Options:  niro.Options{TopP: niro.TopPVal(0.95)},
	})
	topP, _ := body["top_p"].(float64)
	if topP != 0.95 {
		t.Errorf("top_p = %v, want 0.95", topP)
	}
}

func TestBuildParams_TopK(t *testing.T) {
	body := captureRequest(t, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Options:  niro.Options{TopK: niro.TopKVal(40)},
	})
	topK, _ := body["top_k"].(float64)
	if topK != 40 {
		t.Errorf("top_k = %v, want 40", topK)
	}
}

func TestBuildParams_StopSequences(t *testing.T) {
	body := captureRequest(t, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Options:  niro.Options{Stop: []string{"END", "STOP"}},
	})
	stop, ok := body["stop_sequences"].([]any)
	if !ok || len(stop) != 2 {
		t.Fatalf("stop_sequences = %v, want [END STOP]", body["stop_sequences"])
	}
	if stop[0] != "END" || stop[1] != "STOP" {
		t.Errorf("stop_sequences = %v", stop)
	}
}

// ─── buildParams: SystemPrompt ────────────────────────────────────────────────

func TestBuildParams_SystemPrompt(t *testing.T) {
	body := captureRequest(t, &niro.Request{
		SystemPrompt: "You are helpful",
		Messages:     []niro.Message{niro.UserText("hi")},
	})
	system, ok := body["system"].([]any)
	if !ok || len(system) == 0 {
		t.Fatalf("system field = %v, want array with text block", body["system"])
	}
	block := system[0].(map[string]any)
	if block["text"] != "You are helpful" {
		t.Errorf("system text = %v, want 'You are helpful'", block["text"])
	}
}

func TestBuildParams_SystemMessage_InMessages(t *testing.T) {
	// A niro.RoleSystem message in Messages should go to params.System, not Messages
	body := captureRequest(t, &niro.Request{
		Messages: []niro.Message{
			niro.SystemText("inline system"),
			niro.UserText("hi"),
		},
	})
	system, ok := body["system"].([]any)
	if !ok || len(system) == 0 {
		t.Fatalf("system field = %v, expected non-empty", body["system"])
	}
	block := system[0].(map[string]any)
	if block["text"] != "inline system" {
		t.Errorf("system[0].text = %v, want 'inline system'", block["text"])
	}
	// messages should only contain the user message
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("messages len = %d, want 1 (system should be extracted)", len(msgs))
	}
}

func TestBuildParams_SystemPrompt_AndSystemMessage_Combined(t *testing.T) {
	// SystemPrompt + inline system message in Messages — both should appear in system
	body := captureRequest(t, &niro.Request{
		SystemPrompt: "Base instructions",
		Messages: []niro.Message{
			niro.SystemText("extra instructions"),
			niro.UserText("hi"),
		},
	})
	system, ok := body["system"].([]any)
	if !ok {
		t.Fatalf("system = %v, expected array", body["system"])
	}
	// EffectiveMessages prepends SystemPrompt as a system message → both end in system
	if len(system) < 2 {
		t.Errorf("expected >= 2 system blocks (SystemPrompt + inline), got %d", len(system))
	}
}

// ─── buildParams: Tools ───────────────────────────────────────────────────────

func TestBuildParams_Tools(t *testing.T) {
	body := captureRequest(t, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Tools: []niro.Tool{{
			Name:        "search",
			Description: "web search",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
		}},
	})
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools = %v, expected 1 tool", body["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "search" {
		t.Errorf("tool name = %v, want search", tool["name"])
	}
	if tool["description"] != "web search" {
		t.Errorf("tool description = %v, want 'web search'", tool["description"])
	}
	schema, ok := tool["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("input_schema = %v", tool["input_schema"])
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["q"]; !ok {
		t.Errorf("input_schema.properties missing 'q': %v", props)
	}
}

func TestBuildParams_Tools_Enum(t *testing.T) {
	// Verify that tool parameters with enum are passed through (Claude API supports enum in input_schema).
	body := captureRequest(t, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Tools: []niro.Tool{{
			Name:        "get_weather",
			Description: "Get weather",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"unit":{"type":"string","description":"Temperature unit","enum":["celsius","fahrenheit"]}},"required":["unit"]}`),
		}},
	})
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools = %v, expected 1 tool", body["tools"])
	}
	tool := tools[0].(map[string]any)
	schema, ok := tool["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("input_schema = %v", tool["input_schema"])
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		t.Fatal("input_schema.properties missing")
	}
	unit, ok := props["unit"].(map[string]any)
	if !ok {
		t.Fatalf("properties.unit = %v", props["unit"])
	}
	enumVal, ok := unit["enum"].([]any)
	if !ok || len(enumVal) != 2 {
		t.Errorf("properties.unit.enum = %v, want [celsius, fahrenheit]", unit["enum"])
	}
	if unit["description"] != "Temperature unit" {
		t.Errorf("properties.unit.description = %v", unit["description"])
	}
}

func TestBuildParams_Tools_Required(t *testing.T) {
	body := captureRequest(t, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Tools: []niro.Tool{{
			Name:        "fn",
			Description: "d",
			Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":["x","y"]}`),
		}},
	})
	tools := body["tools"].([]any)
	tool := tools[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	req, ok := schema["required"].([]any)
	if !ok || len(req) != 2 {
		t.Errorf("required = %v, want [x, y]", schema["required"])
	}
}

func TestBuildParams_Tools_NoParameters(t *testing.T) {
	body := captureRequest(t, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Tools:    []niro.Tool{{Name: "noop", Description: "does nothing"}},
	})
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, expected 1", body["tools"])
	}
}

// ─── convertMessage: messages ─────────────────────────────────────────────────

func captureMessages(t *testing.T, req *niro.Request) []map[string]any {
	t.Helper()
	body := captureRequest(t, req)
	var result []map[string]any
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				result = append(result, mm)
			}
		}
	}
	return result
}

func TestConvertMessage_UserText(t *testing.T) {
	msgs := captureMessages(t, &niro.Request{
		Messages: []niro.Message{niro.UserText("hello")},
	})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0]["role"] != "user" {
		t.Errorf("role = %v, want user", msgs[0]["role"])
	}
	content, ok := msgs[0]["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %v", msgs[0]["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Errorf("content[0].type = %v, want text", block["type"])
	}
	if block["text"] != "hello" {
		t.Errorf("content[0].text = %v, want hello", block["text"])
	}
}

func TestConvertMessage_UserImage_Data(t *testing.T) {
	msgs := captureMessages(t, &niro.Request{
		Messages: []niro.Message{
			niro.Multi(niro.RoleUser,
				niro.Part{Kind: niro.KindText, Text: "what is this?"},
				niro.Part{Kind: niro.KindImage, Data: []byte{0xFF, 0xD8, 0xFF}, Mime: "image/jpeg"},
			),
		},
	})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	content, ok := msgs[0]["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("expected 2 content blocks, got %v", msgs[0]["content"])
	}
	imgBlock := content[1].(map[string]any)
	if imgBlock["type"] != "image" {
		t.Errorf("type = %v, want image", imgBlock["type"])
	}
	src, ok := imgBlock["source"].(map[string]any)
	if !ok {
		t.Fatalf("source = %v", imgBlock["source"])
	}
	if src["type"] != "base64" {
		t.Errorf("source.type = %v, want base64", src["type"])
	}
	if src["media_type"] != "image/jpeg" {
		t.Errorf("source.media_type = %v, want image/jpeg", src["media_type"])
	}
	if src["data"] == "" {
		t.Error("source.data should not be empty")
	}
}

func TestConvertMessage_UserImage_NoData_Skipped(t *testing.T) {
	// Image part with no data AND no URL — should be skipped (Anthropic needs base64)
	msgs := captureMessages(t, &niro.Request{
		Messages: []niro.Message{
			niro.Multi(niro.RoleUser,
				niro.Part{Kind: niro.KindText, Text: "look"},
				niro.Part{Kind: niro.KindImage}, // no data, no URL
			),
		},
	})
	content, _ := msgs[0]["content"].([]any)
	// Only the text part should remain (image skipped since no data)
	if len(content) != 1 {
		t.Errorf("expected 1 content block (image skipped), got %d", len(content))
	}
}

func TestConvertMessage_AssistantText(t *testing.T) {
	msgs := captureMessages(t, &niro.Request{
		Messages: []niro.Message{
			niro.UserText("hi"),
			niro.AssistantText("hello back"),
		},
	})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[1]["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", msgs[1]["role"])
	}
}

func TestConvertMessage_AssistantWithToolCall(t *testing.T) {
	msgs := captureMessages(t, &niro.Request{
		Messages: []niro.Message{
			niro.UserText("what's the weather?"),
			{
				Role: niro.RoleAssistant,
				Parts: []niro.Part{
					niro.ToolCallPart(&niro.ToolCall{
						ID:   "call-abc",
						Name: "get_weather",
						Args: json.RawMessage(`{"city":"London"}`),
					}),
				},
			},
		},
	})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	asst := msgs[1]
	if asst["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", asst["role"])
	}
	content, ok := asst["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content: %v", asst["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "tool_use" {
		t.Errorf("content[0].type = %v, want tool_use", block["type"])
	}
	if block["id"] != "call-abc" {
		t.Errorf("id = %v, want call-abc", block["id"])
	}
	if block["name"] != "get_weather" {
		t.Errorf("name = %v, want get_weather", block["name"])
	}
}

func TestConvertMessage_AssistantTextAndToolCall(t *testing.T) {
	msgs := captureMessages(t, &niro.Request{
		Messages: []niro.Message{
			niro.UserText("hi"),
			{
				Role: niro.RoleAssistant,
				Parts: []niro.Part{
					{Kind: niro.KindText, Text: "I'll check"},
					niro.ToolCallPart(&niro.ToolCall{ID: "tc1", Name: "fn", Args: json.RawMessage(`{}`)}),
				},
			},
		},
	})
	asst := msgs[1]
	content, _ := asst["content"].([]any)
	if len(content) != 2 {
		t.Errorf("expected 2 content blocks (text + tool_use), got %d", len(content))
	}
}

func TestConvertMessage_ToolResult(t *testing.T) {
	msgs := captureMessages(t, &niro.Request{
		Messages: []niro.Message{
			niro.UserText("weather?"),
			niro.AssistantText("let me check"),
			niro.ToolMessage("call-abc", "Sunny, 25°C"),
		},
	})
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	// Tool result in Anthropic goes to a "user" turn
	toolMsg := msgs[2]
	if toolMsg["role"] != "user" {
		t.Errorf("tool message role = %v, want user (Anthropic puts tool results in user turn)", toolMsg["role"])
	}
	content, ok := toolMsg["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content in tool message: %v", toolMsg["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "tool_result" {
		t.Errorf("type = %v, want tool_result", block["type"])
	}
	if block["tool_use_id"] != "call-abc" {
		t.Errorf("tool_use_id = %v, want call-abc", block["tool_use_id"])
	}
	// Anthropic SDK serializes tool result content as an array of text blocks
	contentArr, ok := block["content"].([]any)
	if !ok || len(contentArr) == 0 {
		t.Fatalf("content = %v, expected array of text blocks", block["content"])
	}
	textBlock, ok := contentArr[0].(map[string]any)
	if !ok || textBlock["text"] != "Sunny, 25°C" {
		t.Errorf("content[0].text = %v, want 'Sunny, 25°C'", contentArr[0])
	}
}

func TestConvertMessage_ToolResult_IsError(t *testing.T) {
	msgs := captureMessages(t, &niro.Request{
		Messages: []niro.Message{
			niro.UserText("call tool"),
			niro.AssistantText("ok"),
			niro.ToolErrorMessage("call-err", "tool failed"),
		},
	})
	toolMsg := msgs[2]
	content, _ := toolMsg["content"].([]any)
	if len(content) == 0 {
		t.Fatal("no content in tool error message")
	}
	block := content[0].(map[string]any)
	if block["is_error"] != true {
		t.Errorf("is_error = %v, want true", block["is_error"])
	}
}

func TestConvertMessage_UnknownRole_TreatedAsUser(t *testing.T) {
	msgs := captureMessages(t, &niro.Request{
		Messages: []niro.Message{
			{Role: "unknown", Parts: []niro.Part{{Kind: niro.KindText, Text: "fallback"}}},
		},
	})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0]["role"] != "user" {
		t.Errorf("role = %v, want user", msgs[0]["role"])
	}
}

// ─── Cache tokens in usage ────────────────────────────────────────────────────

func TestGenerate_CacheTokensInUsage(t *testing.T) {
	// Build a stream that includes cache_read_input_tokens and cache_creation_input_tokens
	var b strings.Builder
	b.WriteString(sseEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":          "id1",
			"type":        "message",
			"role":        "assistant",
			"model":       "claude-sonnet-4-5",
			"content":     []any{},
			"stop_reason": nil,
			"usage": map[string]any{
				"input_tokens":                10,
				"output_tokens":               1,
				"cache_read_input_tokens":     20,
				"cache_creation_input_tokens": 5,
			},
		},
	}))
	b.WriteString(sseEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	}))
	b.WriteString(sseEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "ok"},
	}))
	b.WriteString(sseEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": 0,
	}))
	b.WriteString(sseEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 3},
	}))
	b.WriteString(sseEvent("message_stop", map[string]any{"type": "message_stop"}))

	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, b.String(), ""
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	meta := stream.Response()
	if meta == nil {
		t.Fatal("meta is nil")
	}
	// Detail should contain cache token counts
	if meta.Usage.Detail == nil {
		t.Error("expected Usage.Detail to be populated with cache tokens")
	} else {
		if meta.Usage.Detail["cache_read_input_tokens"] != 20 {
			t.Errorf("cache_read_input_tokens = %d, want 20", meta.Usage.Detail["cache_read_input_tokens"])
		}
		if meta.Usage.Detail["cache_creation_input_tokens"] != 5 {
			t.Errorf("cache_creation_input_tokens = %d, want 5", meta.Usage.Detail["cache_creation_input_tokens"])
		}
		if meta.Usage.Detail[niro.UsageCacheAttempted] != 0 {
			t.Errorf("cache_attempted = %d, want 0", meta.Usage.Detail[niro.UsageCacheAttempted])
		}
	}
}

func TestGenerate_CacheHintAddsCacheControl(t *testing.T) {
	var cacheControlType string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if cc, ok := body["cache_control"].(map[string]any); ok {
			cacheControlType, _ = cc["type"].(string)
		}
		return 200, sseTextStream("id1", "claude-sonnet-4-5", "ok"), ""
	})

	ctx := niro.WithCacheHint(context.Background(), niro.CacheHint{
		Mode: niro.CachePrefer,
		Key:  "tenant-a:key",
		TTL:  5 * time.Minute,
	})
	stream, err := p.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if cacheControlType != "ephemeral" {
		t.Fatalf("cache_control.type = %q, want %q", cacheControlType, "ephemeral")
	}
}

func TestGenerate_CacheRequireMissReturnsError(t *testing.T) {
	var b strings.Builder
	b.WriteString(sseEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      "id1",
			"type":    "message",
			"role":    "assistant",
			"model":   "claude-sonnet-4-5",
			"content": []any{},
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 1,
			},
		},
	}))
	b.WriteString(sseEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	}))
	b.WriteString(sseEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "ok"},
	}))
	b.WriteString(sseEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": 0,
	}))
	b.WriteString(sseEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 3},
	}))
	b.WriteString(sseEvent("message_stop", map[string]any{"type": "message_stop"}))

	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, b.String(), ""
	})
	ctx := niro.WithCacheHint(context.Background(), niro.CacheHint{
		Mode: niro.CacheRequire,
		Key:  "tenant-a:key",
		TTL:  5 * time.Minute,
	})
	stream, err := p.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, err = niro.CollectText(context.Background(), stream)
	if err == nil || !strings.Contains(err.Error(), "cache required") {
		t.Fatalf("expected cache required error, got %v", err)
	}
}

func TestCacheCaps(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, sseTextStream("id1", "claude-sonnet-4-5", "ok"), ""
	})
	caps := p.CacheCaps()
	if !caps.SupportsPrefix || !caps.SupportsTTL {
		t.Fatalf("unexpected cache caps: %+v", caps)
	}
}

// ─── Empty stream ─────────────────────────────────────────────────────────────

func TestGenerate_EmptyContentStream(t *testing.T) {
	// Valid stream but with no content blocks (model returned nothing)
	var b strings.Builder
	b.WriteString(sseEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "id1", "type": "message", "role": "assistant",
			"model": "claude-sonnet-4-5", "content": []any{}, "stop_reason": nil,
			"usage": map[string]any{"input_tokens": 5, "output_tokens": 0},
		},
	}))
	b.WriteString(sseEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 0},
	}))
	b.WriteString(sseEvent("message_stop", map[string]any{"type": "message_stop"}))

	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, b.String(), ""
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	frames, err := niro.Collect(context.Background(), stream)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if len(frames) != 0 {
		t.Errorf("expected 0 frames, got %d", len(frames))
	}
}

func TestGenerate_NilRequest(t *testing.T) {
	p := New("test-key")
	_, err := p.Generate(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "nil request") {
		t.Fatalf("expected nil request error, got %v", err)
	}
}

func TestGenerate_ExperimentalReasoningUnsupported(t *testing.T) {
	p := New("test-key")
	_, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Options:  niro.Options{ExperimentalReasoning: true},
	})
	if err == nil || !strings.Contains(err.Error(), "experimental reasoning") {
		t.Fatalf("expected experimental reasoning error, got %v", err)
	}
}

func TestGenerate_ConcurrentRequests(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, sseTextStream("id1", "claude-sonnet-4-5", "ok"), ""
	})
	ctx := context.Background()

	var wg sync.WaitGroup
	errCh := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := p.Generate(ctx, &niro.Request{
				Messages: []niro.Message{niro.UserText("hi")},
			})
			if err != nil {
				errCh <- err
				return
			}
			text, err := collectText(t, stream)
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

// TestBuildParams_ToolChoice guards a regression where req.ToolChoice was
// silently ignored by the Anthropic provider. The Anthropic API accepts
// auto / any / tool / none and we now forward each correctly.
func TestBuildParams_ToolChoice(t *testing.T) {
	tools := []niro.Tool{{
		Name: "search", Description: "web", Parameters: json.RawMessage(`{"type":"object"}`),
	}}

	cases := []struct {
		name        string
		choice      niro.ToolChoice
		wantTypeKey string
		wantToolNm  string
	}{
		{"auto", niro.ToolChoiceAuto, "auto", ""},
		{"required", niro.ToolChoiceRequired, "any", ""},
		{"none", niro.ToolChoiceNone, "none", ""},
		{"specific", niro.ToolChoiceFunc("search"), "tool", "search"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body := captureRequest(t, &niro.Request{
				Messages:   []niro.Message{niro.UserText("hi")},
				Tools:      tools,
				ToolChoice: tc.choice,
			})
			tcMap, ok := body["tool_choice"].(map[string]any)
			if !ok {
				t.Fatalf("tool_choice missing or wrong type: %v", body["tool_choice"])
			}
			if tcMap["type"] != tc.wantTypeKey {
				t.Errorf("tool_choice.type = %v, want %q", tcMap["type"], tc.wantTypeKey)
			}
			if tc.wantToolNm != "" && tcMap["name"] != tc.wantToolNm {
				t.Errorf("tool_choice.name = %v, want %q", tcMap["name"], tc.wantToolNm)
			}
		})
	}
}

// TestConsume_ThinkingDeltaIsCustomFrame guards that Anthropic
// extended-thinking deltas surface as KindCustom thinking frames instead
// of being dropped or leaking into KindText.
func TestConsume_ThinkingDeltaIsCustomFrame(t *testing.T) {
	body := sseEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "id1", "type": "message", "role": "assistant",
			"model": "claude-sonnet-4-5", "content": []any{}, "stop_reason": nil,
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		},
	}) +
		sseEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		}) +
		sseEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": "deliberating"},
		}) +
		sseEvent("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": 0,
		}) +
		sseEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": 1,
			"content_block": map[string]any{"type": "text", "text": ""},
		}) +
		sseEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 1,
			"delta": map[string]any{"type": "text_delta", "text": "the answer"},
		}) +
		sseEvent("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": 1,
		}) +
		sseEvent("message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 5},
		}) +
		sseEvent("message_stop", map[string]any{"type": "message_stop"})

	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, body, ""
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("think")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var text strings.Builder
	var thinking string
	for stream.Next(context.Background()) {
		f := stream.Frame()
		switch f.Kind {
		case niro.KindText:
			text.WriteString(f.Text)
		case niro.KindCustom:
			if f.Custom != nil && f.Custom.Type == niro.CustomThinking {
				thinking, _ = f.Custom.Data.(string)
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if text.String() != "the answer" {
		t.Errorf("text frames leaked thinking: %q", text.String())
	}
	if thinking != "deliberating" {
		t.Errorf("expected thinking custom frame, got %q", thinking)
	}
}
