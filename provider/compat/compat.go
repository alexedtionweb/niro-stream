// Package compat implements a Ryn Provider using raw HTTP + SSE.
// No external SDK dependencies — stdlib only.
//
// Use this for:
//   - OpenAI-compatible endpoints (Ollama, vLLM, LiteLLM, etc.)
//   - Environments where you can't install provider SDKs
//   - Lightweight single-binary deployments
//
// For production use with official APIs, prefer the SDK-backed providers
// (provider/openai, provider/anthropic, etc.) which handle auth,
// retries, and API quirks correctly.
//
// Usage:
//
//	llm := compat.New("https://api.openai.com/v1", os.Getenv("OPENAI_API_KEY"))
//	stream, err := llm.Generate(ctx, &ryn.Request{
//	    Model: "gpt-4o",
//	    Messages: []ryn.Message{ryn.UserText("Hello")},
//	})
package compat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"ryn.dev/ryn"
	"ryn.dev/ryn/internal/sse"
)

// Provider implements ryn.Provider using raw HTTP + SSE.
// Compatible with any OpenAI-style chat completions API.
type Provider struct {
	baseURL string
	apiKey  string
	client  *http.Client
	model   string
	headers map[string]string
}

var _ ryn.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithClient sets a custom HTTP client.
func WithClient(c *http.Client) Option {
	return func(p *Provider) { p.client = c }
}

// WithModel sets the default model.
func WithModel(model string) Option {
	return func(p *Provider) { p.model = model }
}

// WithHeader adds a custom HTTP header to all requests.
func WithHeader(key, value string) Option {
	return func(p *Provider) { p.headers[key] = value }
}

// New creates a compat provider.
//
// baseURL should be the API root (e.g. "https://api.openai.com/v1").
// apiKey can be empty for unauthenticated endpoints.
func New(baseURL, apiKey string, opts ...Option) *Provider {
	p := &Provider{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  http.DefaultClient,
		model:   "gpt-4o",
		headers: make(map[string]string),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Generate implements ryn.Provider.
func (p *Provider) Generate(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}

	body, err := json.Marshal(buildRequest(model, req))
	if err != nil {
		return nil, fmt.Errorf("ryn/compat: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ryn/compat: request: %w", err)
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
		return nil, fmt.Errorf("ryn/compat: do: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ryn/compat: status %d: %s", resp.StatusCode, b)
	}

	stream, emitter := ryn.NewStream(32)
	go consume(ctx, resp.Body, emitter)
	return stream, nil
}

func consume(ctx context.Context, body io.ReadCloser, out *ryn.Emitter) {
	defer out.Close()
	defer body.Close()

	reader := sse.NewReader(body)
	var tools []toolAccum

	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			out.Error(fmt.Errorf("ryn/compat: sse: %w", err))
			return
		}

		var c chunk
		if err := json.Unmarshal(ev.Data, &c); err != nil {
			out.Error(fmt.Errorf("ryn/compat: decode: %w", err))
			return
		}

		if len(c.Choices) == 0 {
			// May contain usage data only
			if c.Usage != nil {
				usage := &ryn.Usage{
					InputTokens:  c.Usage.PromptTokens,
					OutputTokens: c.Usage.CompletionTokens,
					TotalTokens:  c.Usage.TotalTokens,
				}
				_ = out.Emit(ctx, ryn.UsageFrame(usage))
			}
			continue
		}

		choice := c.Choices[0]

		if choice.Delta.Content != "" {
			if err := out.Emit(ctx, ryn.TextFrame(choice.Delta.Content)); err != nil {
				return
			}
		}

		for _, tc := range choice.Delta.ToolCalls {
			for tc.Index >= len(tools) {
				tools = append(tools, toolAccum{})
			}
			if tc.ID != "" {
				tools[tc.Index].ID = tc.ID
			}
			if tc.Function.Name != "" {
				tools[tc.Index].Name = tc.Function.Name
			}
			tools[tc.Index].Args.WriteString(tc.Function.Arguments)
		}

		if choice.FinishReason != nil && *choice.FinishReason == "tool_calls" {
			for i := range tools {
				frame := ryn.ToolCallFrame(&ryn.ToolCall{
					ID:   tools[i].ID,
					Name: tools[i].Name,
					Args: json.RawMessage(tools[i].Args.String()),
				})
				if err := out.Emit(ctx, frame); err != nil {
					return
				}
			}
			tools = tools[:0]
		}

		// Usage may coexist with choices in the same chunk
		if c.Usage != nil {
			usage := &ryn.Usage{
				InputTokens:  c.Usage.PromptTokens,
				OutputTokens: c.Usage.CompletionTokens,
				TotalTokens:  c.Usage.TotalTokens,
			}
			_ = out.Emit(ctx, ryn.UsageFrame(usage))
		}
		// Set response meta from final chunk
		if choice.FinishReason != nil {
			meta := &ryn.ResponseMeta{
				ID:           c.ID,
				Model:        c.Model,
				FinishReason: *choice.FinishReason,
			}
			out.SetResponse(meta)
		}
	}
}

// --- Request building ---

func buildRequest(model string, req *ryn.Request) chatRequest {
	cr := chatRequest{
		Model:         model,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		MaxTokens:     req.Options.MaxTokens,
		Temperature:   req.Options.Temperature,
		TopP:          req.Options.TopP,
		Stop:          req.Options.Stop,
	}

	msgs := req.EffectiveMessages()
	for _, msg := range msgs {
		cr.Messages = append(cr.Messages, convertMessage(msg))
	}

	for _, tool := range req.Tools {
		ct := chatTool{Type: "function"}
		ct.Function.Name = tool.Name
		ct.Function.Description = tool.Description
		ct.Function.Parameters = tool.Parameters
		cr.Tools = append(cr.Tools, ct)
	}

	return cr
}

func convertMessage(msg ryn.Message) chatMessage {
	cm := chatMessage{Role: string(msg.Role)}

	if msg.Role == ryn.RoleTool && len(msg.Parts) > 0 && msg.Parts[0].Result != nil {
		r := msg.Parts[0].Result
		cm.ToolCallID = r.CallID
		cm.Content = r.Content
		return cm
	}

	if len(msg.Parts) == 1 && msg.Parts[0].Kind == ryn.KindText {
		cm.Content = msg.Parts[0].Text
		return cm
	}

	var parts []chatContent
	for _, p := range msg.Parts {
		switch p.Kind {
		case ryn.KindText:
			parts = append(parts, chatContent{Type: "text", Text: p.Text})
		case ryn.KindImage:
			url := p.URL
			if url == "" && len(p.Data) > 0 {
				url = "data:" + p.Mime + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
			}
			parts = append(parts, chatContent{
				Type:     "image_url",
				ImageURL: &chatImageURL{URL: url},
			})
		case ryn.KindAudio:
			format := audioFormat(p.Mime)
			parts = append(parts, chatContent{
				Type: "input_audio",
				InputAudio: &chatAudio{
					Data:   base64.StdEncoding.EncodeToString(p.Data),
					Format: format,
				},
			})
		}
	}
	cm.Content = parts

	if msg.Role == ryn.RoleAssistant {
		for _, p := range msg.Parts {
			if p.Kind == ryn.KindToolCall && p.Tool != nil {
				tc := chatToolCall{ID: p.Tool.ID, Type: "function"}
				tc.Function.Name = p.Tool.Name
				tc.Function.Arguments = string(p.Tool.Args)
				cm.ToolCalls = append(cm.ToolCalls, tc)
			}
		}
	}

	return cm
}

func audioFormat(mime string) string {
	switch {
	case strings.Contains(mime, "mp3"):
		return "mp3"
	case strings.Contains(mime, "opus"):
		return "opus"
	case strings.Contains(mime, "flac"):
		return "flac"
	default:
		return "wav"
	}
}

// --- JSON types ---

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	Tools         []chatTool     `json:"tools,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatContent struct {
	Type       string        `json:"type"`
	Text       string        `json:"text,omitempty"`
	ImageURL   *chatImageURL `json:"image_url,omitempty"`
	InputAudio *chatAudio    `json:"input_audio,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

type chatAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *chunkUsage   `json:"usage,omitempty"`
}

type chunkChoice struct {
	Delta struct {
		Content   string      `json:"content"`
		ToolCalls []toolDelta `json:"tool_calls"`
	} `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type chunkUsage struct {
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
