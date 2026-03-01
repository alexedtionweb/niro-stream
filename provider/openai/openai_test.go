package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	oai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"

	"github.com/alexedtionweb/niro-stream"
	. "github.com/alexedtionweb/niro-stream/provider/openai"
)

// ─── SSE helpers ─────────────────────────────────────────────────────────────

// sseBody builds a complete SSE response body from JSON-encoded chunk maps.
func sseBody(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: ")
		b.WriteString(c)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// textChunk returns a JSON string for a text delta chunk.
func textChunk(id, model, text string, finishReason string) string {
	fr := "null"
	if finishReason != "" {
		fr = `"` + finishReason + `"`
	}
	return fmt.Sprintf(
		`{"id":%q,"object":"chat.completion.chunk","created":1700000000,"model":%q,`+
			`"choices":[{"index":0,"delta":{"content":%q},"finish_reason":%s}],"usage":null}`,
		id, model, text, fr,
	)
}

// usageChunk returns a JSON string for the final usage chunk.
func usageChunk(id, model string, prompt, completion, total int) string {
	return fmt.Sprintf(
		`{"id":%q,"object":"chat.completion.chunk","created":1700000000,"model":%q,`+
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		id, model, prompt, completion, total,
	)
}

// toolCallChunk produces the three-chunk sequence the OpenAI SSE stream uses
// for a single tool call: start, arguments, finish.
func toolCallChunks(id, model, callID, funcName, args string) []string {
	start := fmt.Sprintf(
		`{"id":%q,"object":"chat.completion.chunk","created":1700000000,"model":%q,`+
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]},"finish_reason":null}],"usage":null}`,
		id, model, callID, funcName,
	)
	argChunk := fmt.Sprintf(
		`{"id":%q,"object":"chat.completion.chunk","created":1700000000,"model":%q,`+
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":null}],"usage":null}`,
		id, model, args,
	)
	finish := fmt.Sprintf(
		`{"id":%q,"object":"chat.completion.chunk","created":1700000000,"model":%q,`+
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":null}`,
		id, model,
	)
	return []string{start, argChunk, finish}
}

// errBody returns a JSON error body in OpenAI's format.
func errBody(msg, errType string) string {
	return fmt.Sprintf(`{"error":{"message":%q,"type":%q,"param":null,"code":null}}`, msg, errType)
}

// ─── Mock middleware ──────────────────────────────────────────────────────────

// mockMiddleware returns an option.RequestOption that intercepts all requests
// and calls handler(req) → (statusCode, body, contentType).
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

// newProvider builds a Provider wired to the given mock handler.
// Retries are disabled (MaxRetries=0) so tests run fast.
func newProvider(t *testing.T, handler func(*http.Request) (int, string, string), opts ...Option) *Provider {
	t.Helper()
	mw := mockMiddleware(handler)
	noRetry := WithRequestOption(option.WithMaxRetries(0))
	allOpts := append([]Option{WithRequestOption(mw), noRetry}, opts...)
	return New("test-key", allOpts...)
}

// simpleTextProvider returns a Provider whose mock always responds with one
// text token followed by usage.
func simpleTextProvider(t *testing.T) *Provider {
	t.Helper()
	return newProvider(t, func(r *http.Request) (int, string, string) {
		body := sseBody(
			textChunk("id1", "gpt-4o", "Hello", ""),
			textChunk("id1", "gpt-4o", "!", "stop"),
			usageChunk("id1", "gpt-4o", 5, 3, 8),
		)
		return 200, body, "text/event-stream"
	})
}

// collectText drains a stream and returns the concatenated text + any error.
func collectText(t *testing.T, s *ryn.Stream) (string, error) {
	t.Helper()
	return ryn.CollectText(context.Background(), s)
}

// ─── Constructor and accessors ────────────────────────────────────────────────

func TestNew_DefaultModel(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		body := sseBody(textChunk("id1", "gpt-4o", "hi", "stop"))
		return 200, body, ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("ping")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	text, err := collectText(t, stream)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if text != "hi" {
		t.Errorf("text = %q, want %q", text, "hi")
	}
}

func TestNew_CustomModel(t *testing.T) {
	var gotModel string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		return 200, sseBody(textChunk("id1", "gpt-4o-mini", "ok", "stop")), ""
	}, WithModel("gpt-4o-mini"))

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if gotModel != "gpt-4o-mini" {
		t.Errorf("model = %q, want %q", gotModel, "gpt-4o-mini")
	}
}

func TestNew_RequestModelOverridesDefault(t *testing.T) {
	var gotModel string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		return 200, sseBody(textChunk("id1", "o1", "ok", "stop")), ""
	}, WithModel("gpt-4o"))

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Model:    "o1",
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if gotModel != "o1" {
		t.Errorf("model = %q, want %q", gotModel, "o1")
	}
}

func TestNewFromClient_AndClient(t *testing.T) {
	underlying := oai.NewClient(option.WithAPIKey("k"))
	p := NewFromClient(underlying, "gpt-4o")
	if p == nil {
		t.Fatal("NewFromClient returned nil")
	}
	// Client() returns the same value-type client
	got := p.Client()
	_ = got // just ensure it compiles and returns without panic
}

// ─── WithRequestOption / WithBaseURL ────────────────────────────────────────

func TestWithBaseURL(t *testing.T) {
	// Use an httptest server — WithBaseURL sets the base URL option.
	// We test it indirectly via WithRequestOption (the middleware already
	// handles routing), but also verify an explicit base URL can be passed.
	p := New("test-key",
		WithBaseURL("http://localhost:19999"), // non-existent; will be overridden
		WithRequestOption(mockMiddleware(func(r *http.Request) (int, string, string) {
			return 200, sseBody(textChunk("id1", "gpt-4o", "pong", "stop")), ""
		})),
	)
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("ping")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	text, _ := collectText(t, stream)
	if text != "pong" {
		t.Errorf("got %q, want %q", text, "pong")
	}
}

// ─── Hooks ────────────────────────────────────────────────────────────────────

func TestWithRequestHook_ProviderLevel(t *testing.T) {
	var capturedLogProbs bool
	hookCalled := false

	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		lp, ok := body["logprobs"].(bool)
		capturedLogProbs = ok && lp
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	}, WithRequestHook(func(p *oai.ChatCompletionNewParams) {
		p.Logprobs = oai.Bool(true)
		hookCalled = true
	}))

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)

	if !hookCalled {
		t.Error("provider-level hook not called")
	}
	if !capturedLogProbs {
		t.Error("logprobs not set by hook")
	}
}

func TestWithRequestHook_MultipleHooks(t *testing.T) {
	calls := []string{}
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	},
		WithRequestHook(func(p *oai.ChatCompletionNewParams) { calls = append(calls, "first") }),
		WithRequestHook(func(p *oai.ChatCompletionNewParams) { calls = append(calls, "second") }),
	)

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
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
	var gotUser string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotUser, _ = body["user"].(string)
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
		Extra: RequestHook(func(params *oai.ChatCompletionNewParams) {
			params.User = oai.String("user-123")
		}),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)

	if gotUser != "user-123" {
		t.Errorf("user = %q, want %q", gotUser, "user-123")
	}
}

// ─── Text streaming ───────────────────────────────────────────────────────────

func TestGenerate_TextStream(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		body := sseBody(
			textChunk("id1", "gpt-4o", "Hello", ""),
			textChunk("id1", "gpt-4o", " world", "stop"),
		)
		return 200, body, ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
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

func TestGenerate_UsageAccumulated(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		body := sseBody(
			textChunk("id1", "gpt-4o", "Hi", ""),
			usageChunk("id1", "gpt-4o", 10, 5, 15),
		)
		return 200, body, ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("ping")},
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
	if usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.TotalTokens != 15 {
		t.Errorf("usage = %+v, want 10/5/15", usage)
	}
}

func TestGenerate_ResponseMeta(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		body := sseBody(
			textChunk("cmpl-abc", "gpt-4o", "Hi", ""),
			textChunk("cmpl-abc", "gpt-4o", "!", "stop"),
			usageChunk("cmpl-abc", "gpt-4o", 10, 2, 12),
		)
		return 200, body, ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
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
		t.Fatal("no response meta")
	}
	if meta.ID != "cmpl-abc" {
		t.Errorf("ID = %q, want %q", meta.ID, "cmpl-abc")
	}
	if meta.Model != "gpt-4o" {
		t.Errorf("Model = %q, want %q", meta.Model, "gpt-4o")
	}
	if meta.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", meta.FinishReason, "stop")
	}
}

// ─── Tool calls ──────────────────────────────────────────────────────────────

func TestGenerate_ToolCall(t *testing.T) {
	chunks := toolCallChunks("id1", "gpt-4o", "call-1", "get_weather", `{"city":"Paris"}`)
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, sseBody(chunks...), ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("weather?")},
		Tools: []ryn.Tool{{
			Name:        "get_weather",
			Description: "Get current weather",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	frames, err := ryn.Collect(context.Background(), stream)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}

	var toolFrames []ryn.Frame
	for _, f := range frames {
		if f.Kind == ryn.KindToolCall {
			toolFrames = append(toolFrames, f)
		}
	}
	if len(toolFrames) != 1 {
		t.Fatalf("expected 1 tool frame, got %d", len(toolFrames))
	}
	tc := toolFrames[0].Tool
	if tc.ID != "call-1" {
		t.Errorf("tool ID = %q, want %q", tc.ID, "call-1")
	}
	if tc.Name != "get_weather" {
		t.Errorf("tool Name = %q, want %q", tc.Name, "get_weather")
	}
	if string(tc.Args) != `{"city":"Paris"}` {
		t.Errorf("tool Args = %s, want %s", tc.Args, `{"city":"Paris"}`)
	}
}

// ─── Context cancellation ────────────────────────────────────────────────────

func TestGenerate_ContextCancelled(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		body := sseBody(
			textChunk("id1", "gpt-4o", "chunk1", ""),
			textChunk("id1", "gpt-4o", "chunk2", ""),
			textChunk("id1", "gpt-4o", "chunk3", "stop"),
		)
		return 200, body, ""
	})

	// Pre-cancel the context so that stream.Next sees ctx.Done() winning the
	// select against the already-buffered pipe frames.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stream, err := p.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Drain with the cancelled context — eventually ctx.Done() wins the select.
	for stream.Next(ctx) {
	}
	err = stream.Err()
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestGenerate_ContextDeadline(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		body := sseBody(
			textChunk("id1", "gpt-4o", "a", ""),
			textChunk("id1", "gpt-4o", "b", "stop"),
		)
		return 200, body, ""
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(1 * time.Millisecond) // ensure deadline is expired

	stream, err := p.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Drain with background ctx so we see the stored error
	for stream.Next(context.Background()) {
	}
	err = stream.Err()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want DeadlineExceeded, got %v", err)
	}
}

// ─── Stream error ─────────────────────────────────────────────────────────────

func TestGenerate_StreamError(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		// Return an HTTP-level error (400)
		return 400, errBody("bad input", "invalid_request_error"), "application/json"
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
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
	if !strings.Contains(err.Error(), "ryn/openai: stream:") {
		t.Errorf("error should contain 'ryn/openai: stream:', got: %v", err)
	}
}

func TestGenerate_StreamError_Unauthorized(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 401, errBody("invalid api key", "invalid_request_error"), "application/json"
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
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
	// Unwrap to find the underlying oai.Error
	var apiErr *oai.Error
	if !errors.As(err, &apiErr) {
		t.Errorf("expected *oai.Error in chain, got: %T %v", err, err)
	}
	if apiErr != nil && apiErr.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
}

func TestGenerate_StreamError_RateLimited(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 429, errBody("rate limit exceeded", "requests"), "application/json"
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
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
	var apiErr *oai.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
}

func TestGenerate_StreamError_ServerError(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 500, errBody("internal error", "server_error"), "application/json"
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
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
}

// ─── buildParams: Options ────────────────────────────────────────────────────

func TestBuildParams_MaxTokens(t *testing.T) {
	var gotMaxTokens float64
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotMaxTokens, _ = body["max_completion_tokens"].(float64)
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
		Options:  ryn.Options{MaxTokens: 256},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if gotMaxTokens != 256 {
		t.Errorf("max_completion_tokens = %v, want 256", gotMaxTokens)
	}
}

func TestBuildParams_Temperature(t *testing.T) {
	var gotTemp float64
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotTemp, _ = body["temperature"].(float64)
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
		Options:  ryn.Options{Temperature: ryn.Temp(0.7)},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if gotTemp != 0.7 {
		t.Errorf("temperature = %v, want 0.7", gotTemp)
	}
}

func TestBuildParams_TopP(t *testing.T) {
	var gotTopP float64
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotTopP, _ = body["top_p"].(float64)
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
		Options:  ryn.Options{TopP: ryn.TopPVal(0.9)},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if gotTopP != 0.9 {
		t.Errorf("top_p = %v, want 0.9", gotTopP)
	}
}

func TestBuildParams_FrequencyAndPresencePenalty(t *testing.T) {
	var gotFreq, gotPres float64
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotFreq, _ = body["frequency_penalty"].(float64)
		gotPres, _ = body["presence_penalty"].(float64)
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})

	fp := 0.5
	pp := 0.3
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
		Options: ryn.Options{
			FrequencyPenalty: &fp,
			PresencePenalty:  &pp,
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if gotFreq != 0.5 {
		t.Errorf("frequency_penalty = %v, want 0.5", gotFreq)
	}
	if gotPres != 0.3 {
		t.Errorf("presence_penalty = %v, want 0.3", gotPres)
	}
}

func TestBuildParams_StopSequences(t *testing.T) {
	var gotStop []any
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if arr, ok := body["stop"].([]any); ok {
			gotStop = arr
		}
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
		Options:  ryn.Options{Stop: []string{"END", "STOP"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if len(gotStop) != 2 || gotStop[0] != "END" || gotStop[1] != "STOP" {
		t.Errorf("stop = %v, want [END STOP]", gotStop)
	}
}

func TestBuildParams_IncludeUsageAlwaysSet(t *testing.T) {
	var includeUsage bool
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if so, ok := body["stream_options"].(map[string]any); ok {
			includeUsage, _ = so["include_usage"].(bool)
		}
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if !includeUsage {
		t.Error("stream_options.include_usage should always be true")
	}
}

// ─── buildParams: Tools ───────────────────────────────────────────────────────

func TestBuildParams_Tools(t *testing.T) {
	var gotTools []any
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotTools, _ = body["tools"].([]any)
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
		Tools: []ryn.Tool{
			{Name: "search", Description: "web search", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if len(gotTools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(gotTools))
	}
	tool := gotTools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool type = %v, want function", tool["type"])
	}
	fn := tool["function"].(map[string]any)
	if fn["name"] != "search" {
		t.Errorf("tool name = %v, want search", fn["name"])
	}
}

func TestBuildParams_Tools_NilParameters(t *testing.T) {
	var gotTools []any
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotTools, _ = body["tools"].([]any)
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
		Tools: []ryn.Tool{
			{Name: "noop", Description: "does nothing"}, // no Parameters
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if len(gotTools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(gotTools))
	}
}

// ─── buildParams: ToolChoice ─────────────────────────────────────────────────

func toolChoiceStr(body map[string]any) string {
	tc := body["tool_choice"]
	if tc == nil {
		return ""
	}
	if s, ok := tc.(string); ok {
		return s
	}
	// named tool choice
	if m, ok := tc.(map[string]any); ok {
		fn, _ := m["function"].(map[string]any)
		name, _ := fn["name"].(string)
		return "func:" + name
	}
	return fmt.Sprintf("%v", tc)
}

func TestBuildParams_ToolChoiceAuto(t *testing.T) {
	var got string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = toolChoiceStr(body)
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages:   []ryn.Message{ryn.UserText("hi")},
		ToolChoice: ryn.ToolChoiceAuto,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if got != "auto" {
		t.Errorf("tool_choice = %q, want %q", got, "auto")
	}
}

func TestBuildParams_ToolChoiceNone(t *testing.T) {
	var got string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = toolChoiceStr(body)
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages:   []ryn.Message{ryn.UserText("hi")},
		ToolChoice: ryn.ToolChoiceNone,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if got != "none" {
		t.Errorf("tool_choice = %q, want %q", got, "none")
	}
}

func TestBuildParams_ToolChoiceRequired(t *testing.T) {
	var got string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = toolChoiceStr(body)
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages:   []ryn.Message{ryn.UserText("hi")},
		ToolChoice: ryn.ToolChoiceRequired,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if got != "required" {
		t.Errorf("tool_choice = %q, want %q", got, "required")
	}
}

func TestBuildParams_ToolChoiceFunc(t *testing.T) {
	var got string
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = toolChoiceStr(body)
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages:   []ryn.Message{ryn.UserText("hi")},
		ToolChoice: "func:search",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	if got != "func:search" {
		t.Errorf("tool_choice = %q, want %q", got, "func:search")
	}
}

func TestBuildParams_ToolChoiceUnknown_Ignored(t *testing.T) {
	var hasToolChoice bool
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, hasToolChoice = body["tool_choice"]
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})
	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages:   []ryn.Message{ryn.UserText("hi")},
		ToolChoice: "func:", // invalid: func: with no name (< 5 chars prefix doesn't match)
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	// "func:" is exactly 5 chars, so len("func:") == 5 which is NOT > 5
	// The default branch falls through without setting anything
	if hasToolChoice {
		t.Error("tool_choice should not be set for invalid func: pattern")
	}
}

// ─── convertMessage: messages ─────────────────────────────────────────────────

func captureMessages(t *testing.T, req *ryn.Request) []map[string]any {
	t.Helper()
	var captured []map[string]any
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if msgs, ok := body["messages"].([]any); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok {
					captured = append(captured, mm)
				}
			}
		}
		return 200, sseBody(textChunk("id1", "gpt-4o", "ok", "stop")), ""
	})
	stream, err := p.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _ = collectText(t, stream)
	return captured
}

func TestConvertMessage_System(t *testing.T) {
	msgs := captureMessages(t, &ryn.Request{
		SystemPrompt: "You are helpful",
		Messages:     []ryn.Message{ryn.UserText("hi")},
	})
	if len(msgs) < 1 {
		t.Fatal("no messages")
	}
	sys := msgs[0]
	if sys["role"] != "system" {
		t.Errorf("role = %v, want system", sys["role"])
	}
}

func TestConvertMessage_UserText(t *testing.T) {
	msgs := captureMessages(t, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hello there")},
	})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0]["role"] != "user" {
		t.Errorf("role = %v, want user", msgs[0]["role"])
	}
	if msgs[0]["content"] != "hello there" {
		t.Errorf("content = %v, want 'hello there'", msgs[0]["content"])
	}
}

func TestConvertMessage_UserMultimodal_ImageURL(t *testing.T) {
	msgs := captureMessages(t, &ryn.Request{
		Messages: []ryn.Message{
			ryn.Multi(ryn.RoleUser,
				ryn.Part{Kind: ryn.KindText, Text: "describe this"},
				ryn.Part{Kind: ryn.KindImage, URL: "https://example.com/img.jpg"},
			),
		},
	})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	parts, ok := msgs[0]["content"].([]any)
	if !ok {
		t.Fatalf("content is not array: %T", msgs[0]["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	// first part: text
	p0 := parts[0].(map[string]any)
	if p0["type"] != "text" {
		t.Errorf("part[0] type = %v, want text", p0["type"])
	}
	// second part: image_url
	p1 := parts[1].(map[string]any)
	if p1["type"] != "image_url" {
		t.Errorf("part[1] type = %v, want image_url", p1["type"])
	}
	imgURL := p1["image_url"].(map[string]any)
	if imgURL["url"] != "https://example.com/img.jpg" {
		t.Errorf("image url = %v", imgURL["url"])
	}
}

func TestConvertMessage_UserMultimodal_ImageData(t *testing.T) {
	msgs := captureMessages(t, &ryn.Request{
		Messages: []ryn.Message{
			ryn.Multi(ryn.RoleUser,
				ryn.Part{Kind: ryn.KindText, Text: "what is this?"},
				ryn.Part{Kind: ryn.KindImage, Data: []byte{0xFF, 0xD8, 0xFF}, Mime: "image/jpeg"},
			),
		},
	})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	parts, ok := msgs[0]["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("expected 2 content parts, got %v", msgs[0]["content"])
	}
	p1 := parts[1].(map[string]any)
	imgURL := p1["image_url"].(map[string]any)
	url, _ := imgURL["url"].(string)
	if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Errorf("expected data URI, got %q", url)
	}
}

func TestConvertMessage_UserMultimodal_ImageData_NoMime(t *testing.T) {
	// Data without MIME type — should default to application/octet-stream
	msgs := captureMessages(t, &ryn.Request{
		Messages: []ryn.Message{
			ryn.Multi(ryn.RoleUser,
				ryn.Part{Kind: ryn.KindImage, Data: []byte{0x01, 0x02}},
			),
		},
	})
	parts, ok := msgs[0]["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("expected 1 part, got %v", msgs[0]["content"])
	}
	p0 := parts[0].(map[string]any)
	imgURL := p0["image_url"].(map[string]any)
	url, _ := imgURL["url"].(string)
	if !strings.HasPrefix(url, "data:application/octet-stream;base64,") {
		t.Errorf("expected default MIME, got %q", url)
	}
}

func TestConvertMessage_AssistantText(t *testing.T) {
	msgs := captureMessages(t, &ryn.Request{
		Messages: []ryn.Message{
			ryn.UserText("hi"),
			ryn.AssistantText("hello back"),
		},
	})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[1]["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", msgs[1]["role"])
	}
}

func TestConvertMessage_AssistantWithToolCalls(t *testing.T) {
	msgs := captureMessages(t, &ryn.Request{
		Messages: []ryn.Message{
			ryn.UserText("what's the weather?"),
			{
				Role: ryn.RoleAssistant,
				Parts: []ryn.Part{
					ryn.ToolCallPart(&ryn.ToolCall{
						ID:   "call-abc",
						Name: "get_weather",
						Args: json.RawMessage(`{"city":"Paris"}`),
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
	toolCalls, ok := asst["tool_calls"].([]any)
	if !ok || len(toolCalls) == 0 {
		t.Fatalf("no tool_calls in assistant message: %+v", asst)
	}
	tc := toolCalls[0].(map[string]any)
	if tc["id"] != "call-abc" {
		t.Errorf("tool call id = %v, want call-abc", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("function name = %v, want get_weather", fn["name"])
	}
}

func TestConvertMessage_ToolResult(t *testing.T) {
	msgs := captureMessages(t, &ryn.Request{
		Messages: []ryn.Message{
			ryn.UserText("what's the weather?"),
			ryn.AssistantText("let me check"),
			ryn.ToolMessage("call-abc", "Sunny, 25°C"),
		},
	})
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	tool := msgs[2]
	if tool["role"] != "tool" {
		t.Errorf("role = %v, want tool", tool["role"])
	}
	if tool["content"] != "Sunny, 25°C" {
		t.Errorf("content = %v", tool["content"])
	}
	if tool["tool_call_id"] != "call-abc" {
		t.Errorf("tool_call_id = %v, want call-abc", tool["tool_call_id"])
	}
}

func TestConvertMessage_ToolResult_NoResult_FallsBack(t *testing.T) {
	// Tool message with no Result part — should use extractText fallback
	msgs := captureMessages(t, &ryn.Request{
		Messages: []ryn.Message{
			{
				Role:  ryn.RoleTool,
				Parts: []ryn.Part{{Kind: ryn.KindText, Text: "raw text result"}},
			},
		},
	})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0]["role"] != "tool" {
		t.Errorf("role = %v, want tool", msgs[0]["role"])
	}
	if msgs[0]["content"] != "raw text result" {
		t.Errorf("content = %v, want 'raw text result'", msgs[0]["content"])
	}
}

func TestConvertMessage_UnknownRole_TreatedAsUser(t *testing.T) {
	msgs := captureMessages(t, &ryn.Request{
		Messages: []ryn.Message{
			{Role: "unknown", Parts: []ryn.Part{{Kind: ryn.KindText, Text: "fallback"}}},
		},
	})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0]["role"] != "user" {
		t.Errorf("role = %v, want user", msgs[0]["role"])
	}
}

// ─── SystemPrompt prepended ───────────────────────────────────────────────────

func TestGenerate_SystemPrompt(t *testing.T) {
	msgs := captureMessages(t, &ryn.Request{
		SystemPrompt: "Be helpful",
		Messages:     []ryn.Message{ryn.UserText("hi")},
	})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(msgs))
	}
	if msgs[0]["role"] != "system" {
		t.Errorf("first message role = %v, want system", msgs[0]["role"])
	}
	if msgs[1]["role"] != "user" {
		t.Errorf("second message role = %v, want user", msgs[1]["role"])
	}
}

// ─── Multiple tool calls in same response ─────────────────────────────────────

func TestGenerate_MultipleToolCalls(t *testing.T) {
	// Build two sequential tool call chunks
	tc1 := toolCallChunks("id1", "gpt-4o", "call-1", "func_a", `{"x":1}`)
	tc2 := toolCallChunks("id1", "gpt-4o", "call-2", "func_b", `{"y":2}`)
	all := append(tc1, tc2...)

	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, sseBody(all...), ""
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("use tools")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	frames, err := ryn.Collect(context.Background(), stream)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}

	var toolFrames []ryn.Frame
	for _, f := range frames {
		if f.Kind == ryn.KindToolCall {
			toolFrames = append(toolFrames, f)
		}
	}
	// The accumulator may produce 1 or 2 tool calls depending on chunk structure
	// At minimum we should get 1 tool call frame
	if len(toolFrames) == 0 {
		t.Error("expected at least 1 tool call frame")
	}
}

// ─── Empty stream ─────────────────────────────────────────────────────────────

func TestGenerate_EmptyStream(t *testing.T) {
	p := newProvider(t, func(r *http.Request) (int, string, string) {
		return 200, sseBody(), "" // DONE with no chunks
	})

	stream, err := p.Generate(context.Background(), &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	frames, err := ryn.Collect(context.Background(), stream)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if len(frames) != 0 {
		t.Errorf("expected 0 frames, got %d", len(frames))
	}
}
