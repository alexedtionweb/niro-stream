// Package openai implements a Ryn Provider for OpenAI-compatible APIs.
//
// It uses only the Go standard library (net/http, encoding/json) and
// the internal SSE reader. No external dependencies.
//
// Supports:
//   - Streaming text generation
//   - Streaming tool calls (accumulated and emitted as complete ToolCallFrames)
//   - Multimodal input (text, image, audio)
//   - Any OpenAI-compatible endpoint (OpenAI, Azure OpenAI, Ollama, vLLM, etc.)
package openai

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

// Provider implements ryn.Provider for OpenAI-compatible APIs.
type Provider struct {
	apiKey  string
	baseURL string
	client  *http.Client
	model   string
}

// Verify interface compliance at compile time.
var _ ryn.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithBaseURL overrides the API base URL.
// Default: "https://api.openai.com/v1"
//
// Use this for Azure OpenAI, Ollama, vLLM, or any compatible endpoint.
func WithBaseURL(url string) Option {
	return func(p *Provider) { p.baseURL = strings.TrimRight(url, "/") }
}

// WithClient sets a custom HTTP client.
func WithClient(c *http.Client) Option {
	return func(p *Provider) { p.client = c }
}

// WithModel sets the default model.
// Can be overridden per-request via Request.Model.
func WithModel(model string) Option {
	return func(p *Provider) { p.model = model }
}

// New creates an OpenAI provider.
func New(apiKey string, opts ...Option) *Provider {
	p := &Provider{
		apiKey:  apiKey,
		baseURL: "https://api.openai.com/v1",
		client:  http.DefaultClient,
		model:   "gpt-4o",
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Generate implements ryn.Provider.
// It opens an SSE connection and streams Frames as they arrive.
func (p *Provider) Generate(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}

	body, err := json.Marshal(buildRequest(model, req))
	if err != nil {
		return nil, fmt.Errorf("ryn/openai: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ryn/openai: request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ryn/openai: do: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ryn/openai: status %d: %s", resp.StatusCode, b)
	}

	stream, emitter := ryn.NewStream(32)
	go p.consume(ctx, resp.Body, emitter)
	return stream, nil
}

// consume reads SSE events from the response body, parses them,
// and emits Frames to the stream. Runs in its own goroutine.
func (p *Provider) consume(ctx context.Context, body io.ReadCloser, out *ryn.Emitter) {
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
			out.Error(fmt.Errorf("ryn/openai: sse: %w", err))
			return
		}

		var c chunk
		if err := json.Unmarshal(ev.Data, &c); err != nil {
			out.Error(fmt.Errorf("ryn/openai: decode: %w", err))
			return
		}

		if len(c.Choices) == 0 {
			continue
		}
		choice := c.Choices[0]

		// --- Text tokens ---
		if choice.Delta.Content != "" {
			if err := out.Emit(ctx, ryn.TextFrame(choice.Delta.Content)); err != nil {
				return
			}
		}

		// --- Tool calls (accumulated across chunks) ---
		for _, tc := range choice.Delta.ToolCalls {
			// Grow accumulator slice as needed
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

		// Emit complete tool calls when the model signals it's done
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
	}

	// Emit end-of-turn signal
	_ = out.Emit(ctx, ryn.ControlFrame(ryn.SignalEOT))
}

// ──────────────────────────────────────────────────────────
// Request building — converts ryn types to OpenAI JSON format
// ──────────────────────────────────────────────────────────

func buildRequest(model string, req *ryn.Request) chatRequest {
	cr := chatRequest{
		Model:       model,
		Stream:      true,
		MaxTokens:   req.Options.MaxTokens,
		Temperature: req.Options.Temperature,
		TopP:        req.Options.TopP,
		Stop:        req.Options.Stop,
	}

	for _, msg := range req.Messages {
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

	// Tool result message
	if msg.Role == ryn.RoleTool && len(msg.Parts) > 0 && msg.Parts[0].Result != nil {
		r := msg.Parts[0].Result
		cm.ToolCallID = r.CallID
		cm.Content = r.Content
		return cm
	}

	// Simple text-only message (most common)
	if len(msg.Parts) == 1 && msg.Parts[0].Kind == ryn.KindText {
		cm.Content = msg.Parts[0].Text
		return cm
	}

	// Multimodal message — convert each part
	var parts []chatContent
	for _, p := range msg.Parts {
		switch p.Kind {
		case ryn.KindText:
			parts = append(parts, chatContent{
				Type: "text",
				Text: p.Text,
			})

		case ryn.KindImage:
			dataURL := "data:" + p.Mime + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
			parts = append(parts, chatContent{
				Type:     "image_url",
				ImageURL: &chatImageURL{URL: dataURL},
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

	// Attach tool calls from assistant messages
	if msg.Role == ryn.RoleAssistant {
		for _, p := range msg.Parts {
			if p.Kind == ryn.KindToolCall && p.Tool != nil {
				tc := chatToolCall{
					ID:   p.Tool.ID,
					Type: "function",
				}
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

// ──────────────────────────────────────────────────────────
// OpenAI API JSON types (internal)
// ──────────────────────────────────────────────────────────

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Tools       []chatTool    `json:"tools,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	Stop        []string      `json:"stop,omitempty"`
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

// --- Streaming response types ---

type chunk struct {
	Choices []chunkChoice `json:"choices"`
}

type chunkChoice struct {
	Delta struct {
		Content   string      `json:"content"`
		ToolCalls []toolDelta `json:"tool_calls"`
	} `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type toolDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toolAccum accumulates streaming tool call data across SSE chunks.
type toolAccum struct {
	ID   string
	Name string
	Args strings.Builder
}
