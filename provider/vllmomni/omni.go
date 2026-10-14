// Package vllmomni implements a Niro Provider for vLLM-Omni's OpenAI-compatible
// chat completions API.
//
// Text output is the default. Audio generation is opt-in via [Modalities]
// because the talker and codec stages dominate latency.
//
//	llm := vllmomni.New(os.Getenv("VLLM_BASE_URL"), "",
//	    vllmomni.WithModel(os.Getenv("MODEL")),
//	    vllmomni.WithModalities("text", "audio"),
//	)
//	stream, err := llm.Generate(ctx, &niro.Request{
//	    Messages: []niro.Message{niro.Multi(niro.RoleUser,
//	        niro.AudioPart(wav, niro.AudioWAV),
//	        niro.TextPart("What is this audio about?"),
//	    )},
//	})
//
// Audio, image, and video parts are sent as audio_url, image_url, and
// video_url. A part with URL set is forwarded as-is; inline bytes become a
// data URL. Streaming audio chunks are base64-decoded directly into
// [niro.KindAudio] frames.
//
// Per-request control is [Params] in Request.Extra (modalities, speaker,
// sampling_params_list, mm_processor_kwargs, extra JSON fields).
//
// Realtime speech-to-speech against the same server uses provider/realtime
// [realtime.NewOmni].
package vllmomni

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/internal/sse"
	"github.com/alexedtionweb/niro-stream/transport"
)

const (
	// maxErrorBodyBytes caps a non-2xx body copied into the error string.
	maxErrorBodyBytes = 64 << 10
	// maxSSELine is large enough for a multi-second base64 waveform chunk.
	maxSSELine           = 16 << 20
	maxToolCallsPerChunk = 1024
)

// Params is optional per-request configuration passed as Request.Extra.
type Params struct {
	// Modalities overrides the provider default ("text" unless WithModalities
	// was set). Use "text", "audio", or both.
	Modalities []string
	// Speaker selects a Qwen talker voice when the server supports it.
	Speaker string
	// SamplingParamsList is the raw JSON array forwarded to the server.
	SamplingParamsList json.RawMessage
	// MMProcessorKwargs is the raw JSON object for the multimodal processor.
	MMProcessorKwargs json.RawMessage
	// Extra is merged into the top-level JSON body. Keys here override
	// fields this provider would otherwise set, except model and messages.
	Extra map[string]any
}

// Option configures a Provider.
type Option func(*Provider)

// Provider implements niro.Provider for vLLM-Omni chat completions.
type Provider struct {
	baseURL    string
	apiKey     string
	client     *http.Client
	model      string
	modalities []string
	speaker    string
	headers    map[string]string
}

var _ niro.Provider = (*Provider)(nil)

// WithClient sets the HTTP client. The default is [transport.DefaultHTTPClient].
func WithClient(c *http.Client) Option {
	return func(p *Provider) { p.client = c }
}

// WithModel sets the default model.
func WithModel(model string) Option {
	return func(p *Provider) { p.model = model }
}

// WithModalities sets the default output modalities. Text-only skips the
// audio stages and is the latency default.
func WithModalities(modalities ...string) Option {
	return func(p *Provider) { p.modalities = append([]string(nil), modalities...) }
}

// WithSpeaker sets a default talker speaker.
func WithSpeaker(speaker string) Option {
	return func(p *Provider) { p.speaker = speaker }
}

// WithHeader adds a request header.
func WithHeader(key, value string) Option {
	return func(p *Provider) { p.headers[key] = value }
}

// New creates a provider. baseURL is the API root, with or without /v1.
// apiKey may be empty. Model is omitted unless WithModel or Request.Model sets it.
func New(baseURL, apiKey string, opts ...Option) *Provider {
	p := &Provider{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		client:     transport.DefaultHTTPClient,
		modalities: []string{"text"},
		headers:    make(map[string]string),
	}
	for _, o := range opts {
		if o != nil {
			o(p)
		}
	}
	return p
}

// Generate implements niro.Provider.
func (p *Provider) Generate(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
	if req == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "nil request")
	}
	model := req.Model
	if model == "" {
		model = p.model
	}
	body, err := niro.JSONMarshal(p.buildRequest(model, req))
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "marshal", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		chatEndpoint(p.baseURL), bytes.NewReader(body))
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for k, v := range p.headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "do", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, niro.NewErrorf(niro.ConvertHTTPStatusToCode(resp.StatusCode), "status %d: %s", resp.StatusCode, string(b))
	}
	stream, emitter := niro.NewStream(niro.DefaultStreamBuffer)
	go consume(ctx, resp.Body, emitter)
	return stream, nil
}

func consume(ctx context.Context, body io.ReadCloser, out *niro.Emitter) {
	defer out.Close()
	defer body.Close()

	reader := sse.NewReaderLimit(body, maxSSELine)
	var tools []toolAccum
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			out.Error(niro.WrapError(niro.ErrCodeStreamError, "sse", err))
			return
		}
		var chunk omniChunk
		if err := niro.JSONUnmarshal(ev.Data, &chunk); err != nil {
			out.Error(niro.WrapError(niro.ErrCodeStreamError, "decode", err))
			return
		}
		if len(chunk.Choices) == 0 {
			if chunk.Usage != nil {
				emitUsage(ctx, out, chunk.Usage)
			}
			continue
		}
		var finish string
		for i := range chunk.Choices {
			choice := &chunk.Choices[i]
			if err := emitChoice(ctx, out, choice, &tools); err != nil {
				return
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = *choice.FinishReason
			}
		}
		if chunk.Usage != nil {
			emitUsage(ctx, out, chunk.Usage)
		}
		if finish != "" {
			out.SetResponse(&niro.ResponseMeta{
				ID:           chunk.ID,
				Model:        chunk.Model,
				FinishReason: finish,
			})
		}
	}
}

func emitChoice(ctx context.Context, out *niro.Emitter, choice *omniChoice, tools *[]toolAccum) error {
	deltas := []*omniDelta{&choice.Delta}
	if choice.Message != nil {
		deltas = append(deltas, choice.Message)
	}
	for _, d := range deltas {
		if d == nil {
			continue
		}
		if d.Content != "" {
			if err := out.Emit(ctx, niro.TextFrame(d.Content)); err != nil {
				return err
			}
		} else if d.Audio != nil && d.Audio.Transcript != "" {
			if err := out.Emit(ctx, niro.TextFrame(d.Audio.Transcript)); err != nil {
				return err
			}
		}
		if d.Audio != nil && len(d.Audio.Data) > 0 {
			meta := choice.AudioMetadata
			if meta == nil {
				meta = d.Audio.Metadata
			}
			if err := out.Emit(ctx, niro.AudioFrame([]byte(d.Audio.Data), audioMIME(meta, d.Audio.Data))); err != nil {
				return err
			}
		}
		for _, tc := range d.ToolCalls {
			if tc.Index < 0 || tc.Index >= maxToolCallsPerChunk {
				out.Error(niro.NewErrorf(niro.ErrCodeStreamError,
					"tool_call index %d out of range [0,%d)", tc.Index, maxToolCallsPerChunk))
				return niro.ErrClosed
			}
			for tc.Index >= len(*tools) {
				*tools = append(*tools, toolAccum{})
			}
			acc := &(*tools)[tc.Index]
			if tc.ID != "" {
				acc.ID = tc.ID
			}
			if tc.Function.Name != "" {
				acc.Name = tc.Function.Name
			}
			acc.Args.WriteString(tc.Function.Arguments)
		}
	}
	if choice.FinishReason != nil && *choice.FinishReason == "tool_calls" {
		for i := range *tools {
			acc := &(*tools)[i]
			if err := out.Emit(ctx, niro.ToolCallFrame(&niro.ToolCall{
				ID:   acc.ID,
				Name: acc.Name,
				Args: json.RawMessage(acc.Args.String()),
			})); err != nil {
				return err
			}
		}
		*tools = (*tools)[:0]
	}
	return nil
}

func emitUsage(ctx context.Context, out *niro.Emitter, u *omniUsage) {
	_ = out.Emit(ctx, niro.UsageFrame(&niro.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}))
}

func audioMIME(meta *audioMeta, data []byte) string {
	format := ""
	rate := 24000
	channels := 1
	if meta != nil {
		format = strings.ToLower(meta.Format)
		if meta.SampleRateHz > 0 {
			rate = meta.SampleRateHz
		}
		if meta.Channels > 0 {
			channels = meta.Channels
		}
	}
	if format == "" && len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE" {
		return niro.AudioWAV
	}
	switch format {
	case "wav", "wave":
		return niro.AudioWAV
	case "mp3", "mpeg":
		return niro.AudioMP3
	case "opus", "ogg":
		return niro.AudioOGGOpus
	case "flac":
		return niro.AudioFLAC
	case "aac":
		return niro.AudioAAC
	}
	return "audio/pcm;rate=" + strconv.Itoa(rate) + ";bits=16;channels=" + strconv.Itoa(channels)
}

func (p *Provider) buildRequest(model string, req *niro.Request) map[string]any {
	params := paramsOf(req.Extra)
	mods := p.modalities
	speaker := p.speaker
	body := map[string]any{
		"stream": true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	if model != "" {
		body["model"] = model
	}
	if req.Options.MaxTokens > 0 {
		body["max_tokens"] = req.Options.MaxTokens
	}
	if req.Options.Temperature != nil {
		body["temperature"] = *req.Options.Temperature
	}
	if req.Options.TopP != nil {
		body["top_p"] = *req.Options.TopP
	}
	if len(req.Options.Stop) > 0 {
		body["stop"] = req.Options.Stop
	}
	msgs := req.EffectiveMessages()
	messages := make([]any, 0, len(msgs))
	for _, msg := range msgs {
		messages = append(messages, convertMessage(msg))
	}
	body["messages"] = messages
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			fn := map[string]any{
				"name":        t.Name,
				"description": t.Description,
			}
			if len(t.Parameters) > 0 {
				fn["parameters"] = t.Parameters
			}
			tools[i] = map[string]any{"type": "function", "function": fn}
		}
		body["tools"] = tools
	}
	if params != nil {
		if len(params.Modalities) > 0 {
			mods = params.Modalities
		}
		if params.Speaker != "" {
			speaker = params.Speaker
		}
		if len(params.SamplingParamsList) > 0 {
			body["sampling_params_list"] = params.SamplingParamsList
		}
		if len(params.MMProcessorKwargs) > 0 {
			body["mm_processor_kwargs"] = params.MMProcessorKwargs
		}
	}
	if len(mods) > 0 {
		body["modalities"] = mods
	}
	if speaker != "" {
		body["speaker"] = speaker
	}
	if params != nil {
		for k, v := range params.Extra {
			if k == "model" || k == "messages" {
				continue
			}
			body[k] = v
		}
	}
	return body
}

func paramsOf(extra any) *Params {
	switch v := extra.(type) {
	case Params:
		return &v
	case *Params:
		return v
	default:
		return nil
	}
}

func convertMessage(msg niro.Message) map[string]any {
	out := map[string]any{"role": string(msg.Role)}
	if msg.Role == niro.RoleTool && len(msg.Parts) > 0 && msg.Parts[0].Result != nil {
		r := msg.Parts[0].Result
		out["tool_call_id"] = r.CallID
		media := captureParts(r)
		if len(media) == 0 {
			out["content"] = nonEmpty(r.Content)
			return out
		}
		content := make([]any, 0, 1+len(media))
		content = append(content, map[string]any{"type": "text", "text": nonEmpty(r.Content)})
		content = append(content, media...)
		out["content"] = content
		return out
	}
	textOnly := len(msg.Parts) == 1 && msg.Parts[0].Kind == niro.KindText
	if textOnly {
		out["content"] = nonEmpty(msg.Parts[0].Text)
	} else {
		parts := make([]any, 0, len(msg.Parts))
		for _, part := range msg.Parts {
			if c, ok := convertPart(part); ok {
				parts = append(parts, c)
			}
		}
		if len(parts) == 0 {
			out["content"] = " "
		} else {
			out["content"] = parts
		}
	}
	if msg.Role == niro.RoleAssistant {
		var calls []any
		for _, part := range msg.Parts {
			if part.Kind == niro.KindToolCall && part.Tool != nil {
				calls = append(calls, map[string]any{
					"id":   part.Tool.ID,
					"type": "function",
					"function": map[string]any{
						"name":      part.Tool.Name,
						"arguments": string(part.Tool.Args),
					},
				})
			}
		}
		if len(calls) > 0 {
			out["tool_calls"] = calls
		}
	}
	return out
}

func captureParts(r *niro.ToolResult) []any {
	if r == nil || len(r.Parts) == 0 {
		return nil
	}
	out := make([]any, 0, len(r.Parts))
	for _, p := range r.Parts {
		if c, ok := convertPart(p); ok {
			out = append(out, c)
		}
	}
	return out
}

func convertPart(part niro.Part) (map[string]any, bool) {
	switch part.Kind {
	case niro.KindText:
		return map[string]any{"type": "text", "text": nonEmpty(part.Text)}, true
	case niro.KindImage:
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": mediaURL(part, "image/png")},
		}, true
	case niro.KindAudio:
		return map[string]any{
			"type":      "audio_url",
			"audio_url": map[string]any{"url": mediaURL(part, niro.AudioWAV)},
		}, true
	case niro.KindVideo:
		return map[string]any{
			"type":      "video_url",
			"video_url": map[string]any{"url": mediaURL(part, "video/mp4")},
		}, true
	default:
		return nil, false
	}
}

func mediaURL(part niro.Part, fallbackMIME string) string {
	if part.URL != "" {
		return part.URL
	}
	mime := part.Mime
	if mime == "" {
		mime = fallbackMIME
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(part.Data)
}

// chatEndpoint accepts a server root, a /v1 base, or a full chat completions URL.
func chatEndpoint(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	switch {
	case strings.HasSuffix(base, "/chat/completions"):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/chat/completions"
	default:
		return base + "/v1/chat/completions"
	}
}

func nonEmpty(s string) string {
	if s == "" {
		return " "
	}
	return s
}

type omniChunk struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []omniChoice `json:"choices"`
	Usage   *omniUsage   `json:"usage"`
}

type omniChoice struct {
	Delta         omniDelta  `json:"delta"`
	Message       *omniDelta `json:"message"`
	FinishReason  *string    `json:"finish_reason"`
	AudioMetadata *audioMeta `json:"audio_metadata"`
}

type omniDelta struct {
	Content   string      `json:"content"`
	Audio     *omniAudio  `json:"audio"`
	ToolCalls []toolDelta `json:"tool_calls"`
}

type omniAudio struct {
	Data       b64Bytes   `json:"data"`
	Transcript string     `json:"transcript"`
	Metadata   *audioMeta `json:"audio_metadata"`
}

type audioMeta struct {
	Format       string `json:"format"`
	SampleRateHz int    `json:"sample_rate_hz"`
	Channels     int    `json:"channels"`
}

type omniUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type toolDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolAccum struct {
	ID   string
	Name string
	Args strings.Builder
}

// b64Bytes decodes a JSON string of standard base64 into raw bytes during
// unmarshal, skipping the intermediate Go string.
type b64Bytes []byte

func (b *b64Bytes) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*b = nil
		return nil
	}
	if data[0] != '"' || data[len(data)-1] != '"' {
		return niro.NewError(niro.ErrCodeStreamError, "audio data is not a JSON string")
	}
	raw := data[1 : len(data)-1]
	buf := make([]byte, base64.StdEncoding.DecodedLen(len(raw)))
	n, err := base64.StdEncoding.Decode(buf, raw)
	if err != nil {
		return err
	}
	*b = buf[:n]
	return nil
}
