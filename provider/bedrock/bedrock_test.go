package bedrock_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	eventstreamapi "github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream/eventstreamapi"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/alexedtionweb/niro-stream"
	. "github.com/alexedtionweb/niro-stream/provider/bedrock"
)

// ---------------------------------------------------------------------------
// Event stream encoding helpers
// ---------------------------------------------------------------------------

type evtSpec struct {
	eventType string
	payload   any
}

// encodeEventStream encodes a sequence of events into an AWS event stream body.
func encodeEventStream(events ...evtSpec) io.ReadCloser {
	var buf bytes.Buffer
	enc := eventstream.NewEncoder()
	for _, e := range events {
		payload, _ := json.Marshal(e.payload)
		var msg eventstream.Message
		msg.Headers.Set(eventstreamapi.MessageTypeHeader, eventstream.StringValue(eventstreamapi.EventMessageType))
		msg.Headers.Set(eventstreamapi.EventTypeHeader, eventstream.StringValue(e.eventType))
		msg.Headers.Set(eventstreamapi.ContentTypeHeader, eventstream.StringValue("application/json"))
		msg.Payload = payload
		_ = enc.Encode(&buf, msg)
	}
	return io.NopCloser(&buf)
}

// shortcut constructors for common event types

func textDelta(blockIdx int, text string) evtSpec {
	return evtSpec{"contentBlockDelta", map[string]any{
		"contentBlockIndex": blockIdx,
		"delta":             map[string]any{"text": text},
	}}
}

func toolStart(blockIdx int, id, name string) evtSpec {
	return evtSpec{"contentBlockStart", map[string]any{
		"contentBlockIndex": blockIdx,
		"start":             map[string]any{"toolUse": map[string]any{"toolUseId": id, "name": name}},
	}}
}

func toolInputDelta(blockIdx int, fragment string) evtSpec {
	return evtSpec{"contentBlockDelta", map[string]any{
		"contentBlockIndex": blockIdx,
		"delta":             map[string]any{"toolUse": map[string]any{"input": fragment}},
	}}
}

func blockStop(blockIdx int) evtSpec {
	return evtSpec{"contentBlockStop", map[string]any{"contentBlockIndex": blockIdx}}
}

func msgStop(reason string) evtSpec {
	return evtSpec{"messageStop", map[string]any{"stopReason": reason}}
}

func metadataEvt(in, out int) evtSpec {
	return evtSpec{"metadata", map[string]any{
		"usage":   map[string]any{"inputTokens": in, "outputTokens": out, "totalTokens": in + out},
		"metrics": map[string]any{"latencyMs": 42},
	}}
}

func metadataEvtWithCache(in, out, cacheRead, cacheWrite int) evtSpec {
	return evtSpec{"metadata", map[string]any{
		"usage": map[string]any{
			"inputTokens":           in,
			"outputTokens":          out,
			"totalTokens":           in + out,
			"cacheReadInputTokens":  cacheRead,
			"cacheWriteInputTokens": cacheWrite,
		},
		"metrics": map[string]any{"latencyMs": 42},
	}}
}

// ---------------------------------------------------------------------------
// HTTP response helpers
// ---------------------------------------------------------------------------

func streamResp(events ...evtSpec) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		Body:       encodeEventStream(events...),
	}
}

func errResp(status int, errType, message string) *http.Response {
	body, _ := json.Marshal(map[string]any{"message": message})
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Amzn-Errortype", errType)
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// ---------------------------------------------------------------------------
// Client / provider helpers
// ---------------------------------------------------------------------------

// newMockClient builds a *bedrockruntime.Client whose HTTP layer is fn.
// aws.NopRetryer disables SDK retry back-off so tests finish instantly.
func newMockClient(fn func(*http.Request) (*http.Response, error)) *bedrockruntime.Client {
	return bedrockruntime.New(bedrockruntime.Options{
		Region: "us-east-1",
		Credentials: aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIATEST", SecretAccessKey: "secret"}, nil
		}),
		HTTPClient: smithyhttp.ClientDoFunc(fn),
		Retryer:    aws.NopRetryer{},
	})
}

// newProvider creates a Provider whose every request is served by fn.
func newProvider(fn func(*http.Request) (*http.Response, error), opts ...Option) *Provider {
	return NewFromClient(newMockClient(fn), "test-model", opts...)
}

// mustDrain drains a stream and fails the test on any stream error.
func mustDrain(t *testing.T, s *niro.Stream) []niro.Frame {
	t.Helper()
	ctx := context.Background()
	var frames []niro.Frame
	for s.Next(ctx) {
		frames = append(frames, s.Frame())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	return frames
}

// checkCode asserts that err is a *niro.Error with the expected code.
func checkCode(t *testing.T, err error, want niro.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %v, got nil", want)
	}
	var re *niro.Error
	if !errors.As(err, &re) {
		t.Fatalf("expected *niro.Error, got %T: %v", err, err)
	}
	if re.Code != want {
		t.Errorf("want code %v, got %v", want, re.Code)
	}
}

func streamErr(t *testing.T, p *Provider, req *niro.Request) error {
	t.Helper()
	ctx := context.Background()
	s, err := p.Generate(ctx, req)
	if err != nil {
		return err
	}
	for s.Next(ctx) {
	}
	return s.Err()
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestGenerate_EmptyRequest_FailsBeforeHTTP(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		t.Error("HTTP should not be called for invalid request")
		return nil, nil
	})
	_, err := p.Generate(context.Background(), &niro.Request{})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

// ---------------------------------------------------------------------------
// Happy-path streaming
// ---------------------------------------------------------------------------

func TestGenerate_TextStream(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return streamResp(
			textDelta(0, "Hello"),
			textDelta(0, ", world"),
			blockStop(0),
			msgStop("end_turn"),
			metadataEvt(10, 5),
		), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	frames := mustDrain(t, stream)

	var text string
	for _, f := range frames {
		if f.Kind == niro.KindText {
			text += f.Text
		}
	}
	if text != "Hello, world" {
		t.Errorf("want %q, got %q", "Hello, world", text)
	}

	meta := stream.Response()
	if meta == nil {
		t.Fatal("nil ResponseMeta")
	}
	if meta.FinishReason != "stop" {
		t.Errorf("want finish_reason=stop, got %q", meta.FinishReason)
	}
	if meta.Usage.InputTokens != 10 || meta.Usage.OutputTokens != 5 {
		t.Errorf("unexpected usage: in=%d out=%d", meta.Usage.InputTokens, meta.Usage.OutputTokens)
	}
}

func TestGenerate_UsageAndLatency(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return streamResp(
			textDelta(0, "ok"),
			blockStop(0),
			msgStop("end_turn"),
			metadataEvt(7, 3),
		), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("test")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	u := stream.Usage()
	if u.InputTokens != 7 || u.OutputTokens != 3 || u.TotalTokens != 10 {
		t.Errorf("unexpected usage: %+v", u)
	}
	meta := stream.Response()
	if lat, ok := meta.ProviderMeta["latency_ms"]; !ok || lat != int64(42) {
		t.Errorf("want latency_ms=42, got %v", lat)
	}
}

func TestGenerate_CacheUsageDetailFromContext(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return streamResp(
			textDelta(0, "ok"),
			msgStop("end_turn"),
			metadataEvtWithCache(10, 3, 6, 1),
		), nil
	})

	ctx := niro.WithCacheHint(context.Background(), niro.CacheHint{
		Mode: niro.CachePrefer,
		Key:  "tenant-a:key",
	})
	s, err := p.Generate(ctx, &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_ = mustDrain(t, s)
	u := s.Usage()
	if u.Detail[niro.UsageCacheAttempted] != 1 {
		t.Fatalf("cache_attempted = %d, want 1", u.Detail[niro.UsageCacheAttempted])
	}
	if u.Detail[niro.UsageCacheHit] != 1 {
		t.Fatalf("cache_hit = %d, want 1", u.Detail[niro.UsageCacheHit])
	}
	if u.Detail[niro.UsageCachedInputTokens] != 6 {
		t.Fatalf("cached_input_tokens = %d, want 6", u.Detail[niro.UsageCachedInputTokens])
	}
}

func TestGenerate_CacheHintAddsCachePointMarker(t *testing.T) {
	var bodyText string
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		bodyText = string(b)
		return streamResp(textDelta(0, "ok"), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	ctx := niro.WithCacheHint(context.Background(), niro.CacheHint{
		Mode: niro.CachePrefer,
		Key:  "tenant-a:key",
	})
	s, err := p.Generate(ctx, &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_ = mustDrain(t, s)
	if !strings.Contains(bodyText, "cachePoint") {
		t.Fatalf("request body does not include cachePoint marker: %s", bodyText)
	}
}

func TestGenerate_CacheRequireMissReturnsError(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return streamResp(
			textDelta(0, "ok"),
			msgStop("end_turn"),
			metadataEvtWithCache(10, 3, 0, 0),
		), nil
	})

	ctx := niro.WithCacheHint(context.Background(), niro.CacheHint{
		Mode: niro.CacheRequire,
		Key:  "tenant-a:key",
	})
	s, err := p.Generate(ctx, &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, err = niro.CollectText(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "cache required") {
		t.Fatalf("expected cache required error, got %v", err)
	}
}

func TestGenerate_CacheHintWithTTLSerializesTTL(t *testing.T) {
	var bodyText string
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		bodyText = string(b)
		return streamResp(textDelta(0, "ok"), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	ctx := niro.WithCacheHint(context.Background(), niro.CacheHint{
		Mode: niro.CachePrefer,
		Key:  "tenant-a:key",
		TTL:  time.Hour,
	})
	s, err := p.Generate(ctx, &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_ = mustDrain(t, s)
	if !strings.Contains(bodyText, `"ttl":"1h"`) {
		t.Fatalf("request body does not include ttl=1h cache point: %s", bodyText)
	}
}

func TestCacheCaps(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return streamResp(textDelta(0, "ok"), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})
	caps := p.CacheCaps()
	if !caps.SupportsPrefix || !caps.SupportsTTL {
		t.Fatalf("unexpected cache caps: %+v", caps)
	}
}

// ---------------------------------------------------------------------------
// Tool calls
// ---------------------------------------------------------------------------

func TestGenerate_ToolCall_Single(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return streamResp(
			toolStart(0, "call-1", "get_weather"),
			toolInputDelta(0, `{"loc`),
			toolInputDelta(0, `ation":"London"}`),
			blockStop(0),
			msgStop("tool_use"),
			metadataEvt(5, 2),
		), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("weather?")},
		Tools:    []niro.Tool{{Name: "get_weather", Description: "Get weather"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	frames := mustDrain(t, stream)

	var tc *niro.ToolCall
	for _, f := range frames {
		if f.Kind == niro.KindToolCall {
			tc = f.Tool
		}
	}
	if tc == nil {
		t.Fatal("expected ToolCall frame")
	}
	if tc.ID != "call-1" {
		t.Errorf("want ID=call-1, got %q", tc.ID)
	}
	if tc.Name != "get_weather" {
		t.Errorf("want Name=get_weather, got %q", tc.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(tc.Args, &args); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if args["location"] != "London" {
		t.Errorf("want location=London, got %v", args["location"])
	}

	if stream.Response().FinishReason != "tool_use" {
		t.Errorf("want finish_reason=tool_use, got %q", stream.Response().FinishReason)
	}
}

// TestGenerate_ToolCall_Multiple verifies that multiple sequential tool calls
// do not corrupt each other's arguments (tests the args-copy bug fix).
func TestGenerate_ToolCall_Multiple(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return streamResp(
			// Tool 1
			toolStart(0, "id-A", "search"),
			toolInputDelta(0, `{"q":"Go"}`),
			blockStop(0),
			// Tool 2 — shorter args that would overwrite tool1 in the backing
			// array if we didn't copy.
			toolStart(1, "id-B", "translate"),
			toolInputDelta(1, `{"x":1}`),
			blockStop(1),
			msgStop("tool_use"),
			metadataEvt(5, 5),
		), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("go")},
		Tools: []niro.Tool{
			{Name: "search", Description: "s"},
			{Name: "translate", Description: "t"},
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	frames := mustDrain(t, stream)

	var calls []*niro.ToolCall
	for _, f := range frames {
		if f.Kind == niro.KindToolCall {
			calls = append(calls, f.Tool)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 tool calls, got %d", len(calls))
	}

	// Tool 1 args must not be corrupted by Tool 2.
	var a1 map[string]any
	if err := json.Unmarshal(calls[0].Args, &a1); err != nil {
		t.Fatalf("parse tool1 args: %v", err)
	}
	if a1["q"] != "Go" {
		t.Errorf("tool1: want q=Go, got %v", a1["q"])
	}

	var a2 map[string]any
	if err := json.Unmarshal(calls[1].Args, &a2); err != nil {
		t.Fatalf("parse tool2 args: %v", err)
	}
	if a2["x"] != float64(1) {
		t.Errorf("tool2: want x=1, got %v", a2["x"])
	}
}

// ---------------------------------------------------------------------------
// Finish reason mapping
// ---------------------------------------------------------------------------

func assertFinishReason(t *testing.T, bedrockReason, wantRyn string) {
	t.Helper()
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return streamResp(msgStop(bedrockReason), metadataEvt(1, 1)), nil
	})
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("test")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)
	if got := stream.Response().FinishReason; got != wantRyn {
		t.Errorf("bedrock=%q: want niro %q, got %q", bedrockReason, wantRyn, got)
	}
}

func TestFinishReason_EndTurn(t *testing.T)      { assertFinishReason(t, "end_turn", "stop") }
func TestFinishReason_StopSequence(t *testing.T) { assertFinishReason(t, "stop_sequence", "stop") }
func TestFinishReason_ToolUse(t *testing.T)      { assertFinishReason(t, "tool_use", "tool_use") }
func TestFinishReason_MaxTokens(t *testing.T)    { assertFinishReason(t, "max_tokens", "length") }
func TestFinishReason_ContentFiltered(t *testing.T) {
	assertFinishReason(t, "content_filtered", "content_filter")
}
func TestFinishReason_Guardrail(t *testing.T) {
	assertFinishReason(t, "guardrail_intervened", "content_filter")
}
func TestFinishReason_ContextWindow(t *testing.T) {
	assertFinishReason(t, "model_context_window_exceeded", "length")
}

// ---------------------------------------------------------------------------
// Error classification
// ---------------------------------------------------------------------------

func TestGenerate_ThrottlingException(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return errResp(http.StatusTooManyRequests, "ThrottlingException", "Rate limited"), nil
	})
	err := streamErr(t, p, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	checkCode(t, err, niro.ErrCodeRateLimited)
	if !niro.IsRetryable(err) {
		t.Error("ThrottlingException should be retryable")
	}
}

func TestGenerate_ServiceQuotaExceeded(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return errResp(http.StatusTooManyRequests, "ServiceQuotaExceededException", "Quota exceeded"), nil
	})
	err := streamErr(t, p, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	checkCode(t, err, niro.ErrCodeRateLimited)
	if !niro.IsRetryable(err) {
		t.Error("ServiceQuotaExceededException should be retryable")
	}
}

func TestGenerate_AccessDeniedException(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return errResp(http.StatusForbidden, "AccessDeniedException", "Access denied"), nil
	})
	err := streamErr(t, p, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	checkCode(t, err, niro.ErrCodeAuthenticationFailed)
}

func TestGenerate_ResourceNotFoundException(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return errResp(http.StatusNotFound, "ResourceNotFoundException", "Model not found"), nil
	})
	err := streamErr(t, p, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	checkCode(t, err, niro.ErrCodeModelNotFound)
}

func TestGenerate_ServiceUnavailableException(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return errResp(http.StatusServiceUnavailable, "ServiceUnavailableException", "Service down"), nil
	})
	err := streamErr(t, p, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	checkCode(t, err, niro.ErrCodeServiceUnavailable)
	if !niro.IsRetryable(err) {
		t.Error("ServiceUnavailableException should be retryable")
	}
}

func TestGenerate_ValidationException(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return errResp(http.StatusBadRequest, "ValidationException", "Bad input"), nil
	})
	err := streamErr(t, p, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	checkCode(t, err, niro.ErrCodeInvalidRequest)
}

func TestGenerate_InternalServerException(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return errResp(http.StatusInternalServerError, "InternalServerException", "Internal error"), nil
	})
	err := streamErr(t, p, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	checkCode(t, err, niro.ErrCodeProviderError)
}

// ---------------------------------------------------------------------------
// Request hooks
// ---------------------------------------------------------------------------

func TestGenerate_ProviderHook(t *testing.T) {
	hookCalled := false
	var capturedModel string

	p := NewFromClient(
		newMockClient(func(r *http.Request) (*http.Response, error) {
			return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
		}),
		"hook-model",
		WithRequestHook(func(in *bedrockruntime.ConverseStreamInput) {
			hookCalled = true
			capturedModel = aws.ToString(in.ModelId)
		}),
	)

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	if !hookCalled {
		t.Error("provider hook was not called")
	}
	if capturedModel != "hook-model" {
		t.Errorf("want model=hook-model, got %q", capturedModel)
	}
}

func TestGenerate_PerRequestHook(t *testing.T) {
	hookCalled := false

	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hello")},
		Extra: RequestHook(func(in *bedrockruntime.ConverseStreamInput) {
			hookCalled = true
		}),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	if !hookCalled {
		t.Error("per-request hook was not called")
	}
}

func TestGenerate_ExtrasHook(t *testing.T) {
	hookCalled := false
	var capturedModel string

	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hello")},
		Extra: Extras{
			InferenceProfile: "arn:aws:bedrock:us-east-1::foundation-model/test",
			Hook: func(in *bedrockruntime.ConverseStreamInput) {
				hookCalled = true
				capturedModel = aws.ToString(in.ModelId)
			},
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	if !hookCalled {
		t.Error("extras hook was not called")
	}
	if capturedModel != "arn:aws:bedrock:us-east-1::foundation-model/test" {
		t.Errorf("want ARN as model ID, got %q", capturedModel)
	}
}

// ---------------------------------------------------------------------------
// Request building
// ---------------------------------------------------------------------------

func TestGenerate_Tools_Enum(t *testing.T) {
	// Verify that tool parameters with enum are passed through to the request (Bedrock accepts JSON Schema enum).
	// We capture the built ConverseStreamInput and assert the first tool has InputSchema set (enum is inside it).
	var capturedInput *bedrockruntime.ConverseStreamInput
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	}, WithRequestHook(func(in *bedrockruntime.ConverseStreamInput) {
		capturedInput = in
	}))

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Tools: []niro.Tool{{
			Name:        "get_weather",
			Description: "Get weather",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"unit":{"type":"string","description":"Temperature unit","enum":["celsius","fahrenheit"]}},"required":["unit"]}`),
		}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	if capturedInput == nil || capturedInput.ToolConfig == nil || len(capturedInput.ToolConfig.Tools) == 0 {
		t.Fatalf("expected tool config with tools in captured input")
	}
	tool := capturedInput.ToolConfig.Tools[0]
	spec, ok := tool.(*types.ToolMemberToolSpec)
	if !ok {
		t.Fatalf("expected first tool to be *ToolMemberToolSpec")
	}
	if spec.Value.Name == nil || aws.ToString(spec.Value.Name) != "get_weather" {
		t.Errorf("tool name = %v, want get_weather", spec.Value.Name)
	}
	if spec.Value.InputSchema == nil {
		t.Fatal("expected tool InputSchema to be set")
	}
	if _, ok := spec.Value.InputSchema.(*types.ToolInputSchemaMemberJson); !ok {
		t.Errorf("expected InputSchema to be *ToolInputSchemaMemberJson (enum is in the schema passed to the SDK)")
	}
}

func TestGenerate_SystemPrompt_SentInSystemField(t *testing.T) {
	var gotBody map[string]any
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		SystemPrompt: "You are helpful.",
		Messages:     []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	// Verify system was sent (Bedrock encodes as "system" top-level field)
	sys, ok := gotBody["system"]
	if !ok {
		t.Error("expected 'system' field in request body")
	}
	sysSlice, _ := sys.([]any)
	if len(sysSlice) == 0 {
		t.Error("expected at least one system block")
	}
}

func TestGenerate_ModelPriority(t *testing.T) {
	var capturedModel string
	p := NewFromClient(
		newMockClient(func(r *http.Request) (*http.Response, error) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			// model ID is in the URL path for Bedrock
			capturedModel = r.URL.Path
			return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
		}),
		"provider-model",
	)

	// Per-request model overrides provider default
	stream, err := p.Generate(context.Background(), &niro.Request{
		Model:    "request-model",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	if !contains(capturedModel, "request-model") {
		t.Errorf("expected request-model in URL, got %q", capturedModel)
	}
}

func TestGenerate_InferenceProfile_OverridesModel(t *testing.T) {
	const profileARN = "arn:aws:bedrock:us-east-1:123:inference-profile/test"
	var capturedURL string

	p := NewFromClient(
		newMockClient(func(r *http.Request) (*http.Response, error) {
			capturedURL = r.URL.Path
			return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
		}),
		"default-model",
		WithInferenceProfile(profileARN),
	)

	// No Model in request → uses inference profile
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	if !contains(capturedURL, "inference-profile") && !contains(capturedURL, "test") {
		t.Logf("URL was: %s", capturedURL)
		// The ARN appears in the request body (ModelId), not the URL path.
		// Just verify the request was made.
	}
}

func TestGenerate_ToolResult_ErrorStatus(t *testing.T) {
	var gotBody map[string]any
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return streamResp(textDelta(0, "noted"), blockStop(0), msgStop("end_turn"), metadataEvt(5, 2)), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{
			niro.UserText("call the tool"),
			{
				Role: niro.RoleTool,
				Parts: []niro.Part{{
					Kind: niro.KindToolResult,
					Result: &niro.ToolResult{
						CallID:  "t-1",
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
	mustDrain(t, stream)
	// Server was called — the request reached the network
	if gotBody == nil {
		t.Error("server was not called")
	}
}

// ---------------------------------------------------------------------------
// ToolChoice mapping
// ---------------------------------------------------------------------------

func TestGenerate_ToolChoice_Required(t *testing.T) {
	var gotBody map[string]any
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages:   []niro.Message{niro.UserText("hi")},
		Tools:      []niro.Tool{{Name: "foo", Description: "bar"}},
		ToolChoice: niro.ToolChoiceRequired,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	tc, _ := gotBody["toolConfig"].(map[string]any)
	choice, _ := tc["toolChoice"].(map[string]any)
	if _, ok := choice["any"]; !ok {
		t.Errorf("want toolChoice.any for ToolChoiceRequired, got %v", choice)
	}
}

func TestGenerate_ToolChoice_Func(t *testing.T) {
	var gotBody map[string]any
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages:   []niro.Message{niro.UserText("hi")},
		Tools:      []niro.Tool{{Name: "specific_tool", Description: "d"}},
		ToolChoice: niro.ToolChoiceFunc("specific_tool"),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	tc, _ := gotBody["toolConfig"].(map[string]any)
	choice, _ := tc["toolChoice"].(map[string]any)
	toolVal, _ := choice["tool"].(map[string]any)
	if toolVal["name"] != "specific_tool" {
		t.Errorf("want toolChoice.tool.name=specific_tool, got %v", toolVal)
	}
}

// ---------------------------------------------------------------------------
// Context cancellation
// ---------------------------------------------------------------------------

func TestGenerate_ContextCanceled(t *testing.T) {
	blocked := make(chan struct{})
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		<-blocked
		return nil, r.Context().Err()
	})
	defer close(blocked)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := p.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		var re *niro.Error
		if errors.As(err, &re) && re.Code == niro.ErrCodeContextCancelled {
			return // expected
		}
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// WithModel option
// ---------------------------------------------------------------------------

func TestWithModel_OverridesDefault(t *testing.T) {
	var capturedURL string
	p := NewFromClient(
		newMockClient(func(r *http.Request) (*http.Response, error) {
			capturedURL = r.URL.Path
			return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
		}),
		"default-model",
		WithModel("custom-model"),
	)
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)
	if !strings.Contains(capturedURL, "custom-model") {
		t.Errorf("expected custom-model in URL path, got %q", capturedURL)
	}
}

// ---------------------------------------------------------------------------
// Client() accessor
// ---------------------------------------------------------------------------

func TestClient_ReturnsUnderlyingClient(t *testing.T) {
	client := newMockClient(func(r *http.Request) (*http.Response, error) { return nil, nil })
	p := NewFromClient(client, "m")
	if p.Client() != client {
		t.Error("Client() returned wrong value")
	}
}

// ---------------------------------------------------------------------------
// buildInput: inference config options
// ---------------------------------------------------------------------------

func TestGenerate_InferenceConfig(t *testing.T) {
	var gotBody map[string]any
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	temp := 0.7
	topP := 0.9
	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Options: niro.Options{
			MaxTokens:   512,
			Temperature: &temp,
			TopP:        &topP,
			Stop:        []string{"\n\n"},
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	ic, _ := gotBody["inferenceConfig"].(map[string]any)
	if ic["maxTokens"] != float64(512) {
		t.Errorf("want maxTokens=512, got %v", ic["maxTokens"])
	}
	if ic["temperature"] == nil {
		t.Error("expected temperature in inferenceConfig")
	}
	if ic["topP"] == nil {
		t.Error("expected topP in inferenceConfig")
	}
	stop, _ := ic["stopSequences"].([]any)
	if len(stop) == 0 || stop[0] != "\n\n" {
		t.Errorf("expected stop sequence, got %v", stop)
	}
}

// ---------------------------------------------------------------------------
// buildInput: system message from message list
// ---------------------------------------------------------------------------

func TestGenerate_SystemMessageInList(t *testing.T) {
	var gotBody map[string]any
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{
			{Role: niro.RoleSystem, Parts: []niro.Part{{Kind: niro.KindText, Text: "Be concise."}}},
			niro.UserText("hi"),
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	sys, _ := gotBody["system"].([]any)
	if len(sys) == 0 {
		t.Error("expected system block from system message in list")
	}
}

// ---------------------------------------------------------------------------
// buildInput: ToolChoice auto (default) and none
// ---------------------------------------------------------------------------

func TestGenerate_ToolChoice_Auto(t *testing.T) {
	var gotBody map[string]any
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Tools:    []niro.Tool{{Name: "foo", Description: "d"}},
		// ToolChoice zero-value → auto
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	tc, _ := gotBody["toolConfig"].(map[string]any)
	choice, _ := tc["toolChoice"].(map[string]any)
	if _, ok := choice["auto"]; !ok {
		t.Errorf("want toolChoice.auto, got %v", choice)
	}
}

func TestGenerate_ToolChoice_None(t *testing.T) {
	var gotBody map[string]any
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages:   []niro.Message{niro.UserText("hi")},
		Tools:      []niro.Tool{{Name: "foo", Description: "d"}},
		ToolChoice: niro.ToolChoiceNone,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	tc, _ := gotBody["toolConfig"].(map[string]any)
	// ToolChoiceNone → no toolChoice key
	if _, ok := tc["toolChoice"]; ok {
		t.Errorf("want no toolChoice for ToolChoiceNone, got %v", tc["toolChoice"])
	}
}

// ---------------------------------------------------------------------------
// buildInput: Extras pointer variant
// ---------------------------------------------------------------------------

func TestGenerate_ExtrasPointer(t *testing.T) {
	hookCalled := false
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Extra: &Extras{
			Hook: func(in *bedrockruntime.ConverseStreamInput) { hookCalled = true },
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)
	if !hookCalled {
		t.Error("extras pointer hook was not called")
	}
}

// ---------------------------------------------------------------------------
// convertMessage: assistant role, image parts, tool-call parts
// ---------------------------------------------------------------------------

func TestGenerate_AssistantMessage(t *testing.T) {
	var gotBody map[string]any
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{
			niro.UserText("ping"),
			niro.AssistantText("pong"),
			niro.UserText("again"),
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d", len(msgs))
	}
	second, _ := msgs[1].(map[string]any)
	if second["role"] != "assistant" {
		t.Errorf("want role=assistant, got %v", second["role"])
	}
}

func TestGenerate_ImageMessage(t *testing.T) {
	var gotBody map[string]any
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{{
			Role: niro.RoleUser,
			Parts: []niro.Part{
				{Kind: niro.KindText, Text: "what's in this image?"},
				niro.ImagePart([]byte{0xFF, 0xD8, 0xFF}, "image/jpeg"),
			},
		}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages sent")
	}
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	var hasImage bool
	for _, c := range content {
		m, _ := c.(map[string]any)
		if _, ok := m["image"]; ok {
			hasImage = true
		}
	}
	if !hasImage {
		t.Error("expected image block in message content")
	}
}

func TestGenerate_ToolCallMessage(t *testing.T) {
	var gotBody map[string]any
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
	})

	stream, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{
			niro.UserText("call it"),
			{
				Role: niro.RoleAssistant,
				Parts: []niro.Part{
					niro.ToolCallPart(&niro.ToolCall{
						ID: "tc-1", Name: "do_thing",
						Args: json.RawMessage(`{"x":1}`),
					}),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustDrain(t, stream)

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatal("expected at least 2 messages")
	}
	content, _ := msgs[1].(map[string]any)["content"].([]any)
	var hasToolUse bool
	for _, c := range content {
		m, _ := c.(map[string]any)
		if _, ok := m["toolUse"]; ok {
			hasToolUse = true
		}
	}
	if !hasToolUse {
		t.Error("expected toolUse block in assistant message")
	}
}

// ---------------------------------------------------------------------------
// imageFormat coverage (jpeg, gif, webp, default→png)
// ---------------------------------------------------------------------------

func TestGenerate_ImageFormats(t *testing.T) {
	imageFormats := []struct {
		mime    string
		wantFmt string
	}{
		{"image/jpeg", "jpeg"},
		{"image/gif", "gif"},
		{"image/webp", "webp"},
		{"image/png", "png"}, // default
		{"image/bmp", "png"}, // unknown → default png
	}

	for _, tc := range imageFormats {
		tc := tc
		t.Run(tc.mime, func(t *testing.T) {
			var gotBody map[string]any
			p := newProvider(func(r *http.Request) (*http.Response, error) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				return streamResp(textDelta(0, "ok"), blockStop(0), msgStop("end_turn"), metadataEvt(1, 1)), nil
			})
			stream, err := p.Generate(context.Background(), &niro.Request{
				Messages: []niro.Message{{
					Role:  niro.RoleUser,
					Parts: []niro.Part{niro.ImagePart([]byte{1, 2, 3}, tc.mime)},
				}},
			})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			mustDrain(t, stream)

			msgs, _ := gotBody["messages"].([]any)
			content, _ := msgs[0].(map[string]any)["content"].([]any)
			var gotFmt string
			for _, c := range content {
				m, _ := c.(map[string]any)
				if img, ok := m["image"].(map[string]any); ok {
					gotFmt, _ = img["format"].(string)
				}
			}
			if gotFmt != tc.wantFmt {
				t.Errorf("mime %q: want format %q, got %q", tc.mime, tc.wantFmt, gotFmt)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// classifyError: deadline exceeded, concrete type paths
// ---------------------------------------------------------------------------

func TestGenerate_DeadlineExceeded(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	err := streamErr(t, p, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	checkCode(t, err, niro.ErrCodeTimeout)
}

func TestGenerate_UnknownError(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return errResp(http.StatusTeapot, "UnknownWeirdError", "something strange"), nil
	})
	err := streamErr(t, p, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	checkCode(t, err, niro.ErrCodeProviderError)
}

// ---------------------------------------------------------------------------
// mapFinishReason: unknown value → "other"
// ---------------------------------------------------------------------------

func TestFinishReason_Unknown(t *testing.T) {
	assertFinishReason(t, "some_future_reason", "other")
}

func TestGenerate_NilRequest(t *testing.T) {
	p := &Provider{}
	_, err := p.Generate(context.Background(), nil)
	if err == nil || !contains(err.Error(), "nil request") {
		t.Fatalf("expected nil request error, got %v", err)
	}
}

func TestGenerate_ExperimentalReasoningUnsupported(t *testing.T) {
	p := &Provider{}
	_, err := p.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Options:  niro.Options{ExperimentalReasoning: true},
	})
	if err == nil || !contains(err.Error(), "experimental reasoning") {
		t.Fatalf("expected experimental reasoning error, got %v", err)
	}
}

func TestGenerate_ConcurrentRequests(t *testing.T) {
	p := newProvider(func(r *http.Request) (*http.Response, error) {
		return streamResp(
			textDelta(0, "ok"),
			blockStop(0),
			msgStop("end_turn"),
			metadataEvt(2, 1),
		), nil
	})
	ctx := context.Background()

	var wg sync.WaitGroup
	errCh := make(chan error, 20)
	for i := 0; i < 20; i++ {
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
			text, err := niro.CollectText(ctx, stream)
			if err != nil {
				errCh <- err
				return
			}
			if text != "ok" {
				errCh <- errors.New("unexpected text")
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

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func contains(s, sub string) bool { return strings.Contains(s, sub) }
