// Package anthropic implements a Ryn Provider backed by the official
// Anthropic Go SDK (github.com/anthropics/anthropic-sdk-go).
//
// This provider:
//   - Uses the official SDK for auth, retries, and API compatibility
//   - Streams text tokens and tool calls
//   - Supports multimodal input (text, image)
//   - Reports token usage via KindUsage frames
//   - Sets ResponseMeta with model, finish reason, and response ID
//
// Usage:
//
//	llm := anthropic.New(os.Getenv("ANTHROPIC_API_KEY"))
//	stream, err := llm.Generate(ctx, &ryn.Request{
//	    Model: "claude-sonnet-4-5",
//	    Messages: []ryn.Message{ryn.UserText("Hello")},
//	})
package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	ant "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"ryn.dev/ryn"
)

// Provider implements ryn.Provider using the official Anthropic SDK.
type Provider struct {
	client ant.Client
	model  string
}

var _ ryn.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*providerConfig)

type providerConfig struct {
	opts  []option.RequestOption
	model string
}

// WithModel sets the default model.
func WithModel(model string) Option {
	return func(c *providerConfig) { c.model = model }
}

// WithRequestOption appends a raw SDK request option.
func WithRequestOption(opt option.RequestOption) Option {
	return func(c *providerConfig) {
		c.opts = append(c.opts, opt)
	}
}

// New creates an Anthropic provider.
func New(apiKey string, opts ...Option) *Provider {
	cfg := &providerConfig{model: string(ant.ModelClaudeSonnet4_5)}
	for _, o := range opts {
		o(cfg)
	}
	clientOpts := append([]option.RequestOption{option.WithAPIKey(apiKey)}, cfg.opts...)
	return &Provider{
		client: ant.NewClient(clientOpts...),
		model:  cfg.model,
	}
}

// NewFromClient creates a provider from an existing Anthropic SDK client.
func NewFromClient(client ant.Client, model string) *Provider {
	return &Provider{client: client, model: model}
}

// Generate implements ryn.Provider.
func (p *Provider) Generate(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}

	params := p.buildParams(model, req)
	sdk := p.client.Messages.NewStreaming(ctx, params)

	stream, emitter := ryn.NewStream(32)
	go p.consume(ctx, sdk, emitter)
	return stream, nil
}

func (p *Provider) consume(ctx context.Context, sdk *ssestream.Stream[ant.MessageStreamEventUnion], out *ryn.Emitter) {
	defer out.Close()
	defer sdk.Close()

	message := ant.Message{}

	for sdk.Next() {
		event := sdk.Current()
		message.Accumulate(event)

		switch variant := event.AsAny().(type) {
		case ant.ContentBlockDeltaEvent:
			switch delta := variant.Delta.AsAny().(type) {
			case ant.TextDelta:
				if err := out.Emit(ctx, ryn.TextFrame(delta.Text)); err != nil {
					return
				}
			}
		default:
			// Ignore other event types (MessageStart, ContentBlockStart, etc.)
		}
	}

	if err := sdk.Err(); err != nil {
		out.Error(fmt.Errorf("ryn/anthropic: stream: %w", err))
		return
	}

	// Emit completed tool calls from accumulated message
	for _, block := range message.Content {
		if tu, ok := block.AsAny().(ant.ToolUseBlock); ok {
			tc := &ryn.ToolCall{
				ID:   tu.ID,
				Name: tu.Name,
				Args: json.RawMessage(tu.Input),
			}
			if err := out.Emit(ctx, ryn.ToolCallFrame(tc)); err != nil {
				return
			}
		}
	}

	// Emit usage
	usage := &ryn.Usage{
		InputTokens:  int(message.Usage.InputTokens),
		OutputTokens: int(message.Usage.OutputTokens),
		TotalTokens:  int(message.Usage.InputTokens + message.Usage.OutputTokens),
	}
	if message.Usage.CacheReadInputTokens > 0 || message.Usage.CacheCreationInputTokens > 0 {
		usage.Detail = map[string]int{
			"cache_read_input_tokens":     int(message.Usage.CacheReadInputTokens),
			"cache_creation_input_tokens": int(message.Usage.CacheCreationInputTokens),
		}
	}
	_ = out.Emit(ctx, ryn.UsageFrame(usage))

	// Response metadata
	out.SetResponse(&ryn.ResponseMeta{
		ID:           message.ID,
		Model:        string(message.Model),
		FinishReason: string(message.StopReason),
		Usage:        *usage,
	})
}

func (p *Provider) buildParams(model string, req *ryn.Request) ant.MessageNewParams {
	params := ant.MessageNewParams{
		Model: ant.Model(model),
	}

	// System prompt
	if req.SystemPrompt != "" {
		params.System = []ant.TextBlockParam{
			{Text: req.SystemPrompt},
		}
	}

	// Max tokens (Anthropic requires this)
	maxTokens := req.Options.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096 // Anthropic requires an explicit max
	}
	params.MaxTokens = int64(maxTokens)

	// Messages
	for _, msg := range req.Messages {
		if msg.Role == ryn.RoleSystem {
			// Anthropic: system messages go into params.System
			for _, part := range msg.Parts {
				if part.Kind == ryn.KindText {
					params.System = append(params.System, ant.TextBlockParam{
						Text: part.Text,
					})
				}
			}
			continue
		}
		params.Messages = append(params.Messages, convertMessage(msg))
	}

	// Options
	if req.Options.Temperature != nil {
		params.Temperature = ant.Float(*req.Options.Temperature)
	}
	if req.Options.TopP != nil {
		params.TopP = ant.Float(*req.Options.TopP)
	}
	if req.Options.TopK != nil {
		params.TopK = ant.Int(int64(*req.Options.TopK))
	}
	if len(req.Options.Stop) > 0 {
		params.StopSequences = req.Options.Stop
	}

	// Tools
	for _, tool := range req.Tools {
		schema := ant.ToolInputSchemaParam{}
		if len(tool.Parameters) > 0 {
			var props map[string]any
			_ = json.Unmarshal(tool.Parameters, &props)
			if p, ok := props["properties"]; ok {
				schema.Properties = p
			}
			if r, ok := props["required"].([]any); ok {
				for _, v := range r {
					if s, ok := v.(string); ok {
						schema.Required = append(schema.Required, s)
					}
				}
			}
		}
		params.Tools = append(params.Tools, ant.ToolUnionParam{
			OfTool: &ant.ToolParam{
				Name:        tool.Name,
				Description: ant.String(tool.Description),
				InputSchema: schema,
			},
		})
	}

	return params
}

func convertMessage(msg ryn.Message) ant.MessageParam {
	switch msg.Role {
	case ryn.RoleUser:
		var blocks []ant.ContentBlockParamUnion
		for _, p := range msg.Parts {
			switch p.Kind {
			case ryn.KindText:
				blocks = append(blocks, ant.NewTextBlock(p.Text))
			case ryn.KindImage:
				if len(p.Data) > 0 {
					b64 := base64.StdEncoding.EncodeToString(p.Data)
					blocks = append(blocks, ant.NewImageBlockBase64(p.Mime, b64))
				}
			}
		}
		return ant.NewUserMessage(blocks...)

	case ryn.RoleAssistant:
		var blocks []ant.ContentBlockParamUnion
		for _, p := range msg.Parts {
			switch p.Kind {
			case ryn.KindText:
				blocks = append(blocks, ant.NewTextBlock(p.Text))
			case ryn.KindToolCall:
				if p.Tool != nil {
					var input any
					_ = json.Unmarshal(p.Tool.Args, &input)
					blocks = append(blocks, ant.NewToolUseBlock(p.Tool.ID, input, p.Tool.Name))
				}
			}
		}
		return ant.NewAssistantMessage(blocks...)

	case ryn.RoleTool:
		// Anthropic: tool results are in user turns
		var blocks []ant.ContentBlockParamUnion
		for _, p := range msg.Parts {
			if p.Kind == ryn.KindToolResult && p.Result != nil {
				blocks = append(blocks, ant.NewToolResultBlock(
					p.Result.CallID,
					p.Result.Content,
					p.Result.IsError,
				))
			}
		}
		return ant.NewUserMessage(blocks...)

	default:
		return ant.NewUserMessage(ant.NewTextBlock(extractText(msg)))
	}
}

func extractText(msg ryn.Message) string {
	for _, p := range msg.Parts {
		if p.Kind == ryn.KindText {
			return p.Text
		}
	}
	return ""
}
