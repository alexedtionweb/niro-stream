package openrouter_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
	. "github.com/alexedtionweb/niro-stream/provider/openrouter"
)

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

func textChunk(id, model, text string, finishReason string) string {
	fr := "null"
	if finishReason != "" {
		fr = `"` + finishReason + `"`
	}
	return fmt.Sprintf(
		`{"id":%q,"object":"chat.completion.chunk","created":1700000000,"model":%q,`+
			`"choices":[{"index":0,"delta":{"content":%q},"finish_reason":%s}]}`,
		id, model, text, fr,
	)
}

func usageChunk(id, model string, prompt, completion, total int) string {
	return fmt.Sprintf(
		`{"id":%q,"object":"chat.completion.chunk","created":1700000000,"model":%q,`+
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		id, model, prompt, completion, total,
	)
}

func toolCallChunks(id, model, callID, funcName, args string) []string {
	start := fmt.Sprintf(
		`{"id":%q,"object":"chat.completion.chunk","created":1700000000,"model":%q,`+
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]},"finish_reason":null}]}`,
		id, model, callID, funcName,
	)
	argChunk := fmt.Sprintf(
		`{"id":%q,"object":"chat.completion.chunk","created":1700000000,"model":%q,`+
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":null}]}`,
		id, model, args,
	)
	finish := fmt.Sprintf(
		`{"id":%q,"object":"chat.completion.chunk","created":1700000000,"model":%q,`+
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		id, model,
	)
	return []string{start, argChunk, finish}
}

func mockServer(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	return srv
}

func newTestProvider(t *testing.T, srv *httptest.Server) *Provider {
	t.Helper()
	return New("test-key",
		WithServerURL(srv.URL+"/v1"),
		WithClient(srv.Client()),
		WithModel("openai/gpt-4o"),
	)
}

func TestGenerate_TextStream(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); !strings.Contains(got, "test-key") {
			t.Fatalf("missing auth header: %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("bad request json: %v", err)
		}
		if req["stream"] != true {
			t.Fatalf("expected stream=true, got %#v", req["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(
			textChunk("c1", "openai/gpt-4o", "Hello", ""),
			textChunk("c1", "openai/gpt-4o", " world", ""),
			usageChunk("c1", "openai/gpt-4o", 5, 2, 7),
		))
	})

	p := newTestProvider(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := p.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}

	var text strings.Builder
	for stream.Next(ctx) {
		f := stream.Frame()
		if f.Kind == niro.KindText {
			text.WriteString(f.Text)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if got := text.String(); got != "Hello world" {
		t.Fatalf("text = %q", got)
	}
	u := stream.Usage()
	if u.TotalTokens != 7 {
		t.Fatalf("usage = %+v", u)
	}
	meta := stream.Response()
	if meta == nil || meta.ID != "c1" || meta.Model != "openai/gpt-4o" {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestGenerate_ToolCalls(t *testing.T) {
	chunks := toolCallChunks("c2", "openai/gpt-4o", "call_1", "get_weather", `{"city":"NYC"}`)
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(chunks...))
	})

	p := newTestProvider(t, srv)
	ctx := context.Background()
	stream, err := p.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("weather?")},
		Tools: []niro.Tool{{
			Name:        "get_weather",
			Description: "Get weather",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var calls []*niro.ToolCall
	for stream.Next(ctx) {
		f := stream.Frame()
		if f.Kind == niro.KindToolCall {
			calls = append(calls, f.Tool)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "get_weather" {
		t.Fatalf("call = %+v", calls[0])
	}
	if string(calls[0].Args) != `{"city":"NYC"}` {
		t.Fatalf("args = %s", calls[0].Args)
	}
}

func TestGenerate_ReasoningDelta(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := `{"id":"c3","object":"chat.completion.chunk","created":1,"model":"openai/o1",` +
			`"choices":[{"index":0,"delta":{"reasoning":"step 1"},"finish_reason":null}]}`
		_, _ = io.WriteString(w, sseBody(
			chunk,
			textChunk("c3", "openai/o1", "answer", "stop"),
		))
	})

	p := newTestProvider(t, srv)
	ctx := context.Background()
	stream, err := p.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("think")},
	})
	if err != nil {
		t.Fatal(err)
	}

	var thinking, text string
	for stream.Next(ctx) {
		f := stream.Frame()
		switch f.Kind {
		case niro.KindCustom:
			if f.Custom != nil && f.Custom.Type == niro.CustomThinking {
				thinking, _ = f.Custom.Data.(string)
			}
		case niro.KindText:
			text += f.Text
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if thinking != "step 1" {
		t.Fatalf("thinking = %q", thinking)
	}
	if text != "answer" {
		t.Fatalf("text = %q", text)
	}
}

func TestGenerate_NilRequest(t *testing.T) {
	p := New("key")
	_, err := p.Generate(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestGenerate_ExperimentalReasoningUnsupported(t *testing.T) {
	p := New("key")
	_, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Options:  niro.Options{ExperimentalReasoning: true},
	})
	if err == nil || !strings.Contains(err.Error(), "experimental reasoning") {
		t.Fatalf("expected experimental reasoning error, got %v", err)
	}
}

func TestGenerate_HTTPError(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key","type":"auth"}}`)
	})
	p := newTestProvider(t, srv)
	_, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestWithHTTPRefererAndTitle(t *testing.T) {
	var gotReferer, gotTitle string
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-Title")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(textChunk("c", "openai/gpt-4o", "ok", "stop")))
	})

	p := New("test-key",
		WithServerURL(srv.URL+"/v1"),
		WithClient(srv.Client()),
		WithHTTPReferer("https://example.com"),
		WithXTitle("Niro"),
	)
	ctx := context.Background()
	stream, err := p.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for stream.Next(ctx) {
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if gotReferer != "https://example.com" {
		t.Fatalf("referer = %q", gotReferer)
	}
	if gotTitle != "Niro" {
		t.Fatalf("title = %q", gotTitle)
	}
}
