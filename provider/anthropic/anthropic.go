// Package anthropic implements a Niro Provider backed by the official
// Anthropic Go SDK (github.com/anthropics/anthropic-sdk-go).
//
// This is an opt-in provider module. Import it only when you need
// Anthropic Claude models:
//
//	go get github.com/alexedtionweb/niro-stream/provider/anthropic
//
// # SDK Access
//
// Use [Provider.Client] to access the underlying SDK client directly.
// Use [WithRequestHook] or pass a [RequestHook] as Request.Extra for
// raw SDK param customization.
//
// # Usage
//
//	llm := anthropic.New(os.Getenv("ANTHROPIC_API_KEY"))
//	stream, err := llm.Generate(ctx, &niro.Request{
//	    Model: "claude-sonnet-4-5",
//	    Messages: []niro.Message{niro.UserText("Hello")},
//	})
package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	ant "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/alexedtionweb/niro-stream"
)

// RequestHook allows modifying the raw SDK params before the request is sent.
//
// Provider-level:
//
//	anthropic.New(key, anthropic.WithRequestHook(func(p *ant.MessageNewParams) {
//	    p.Metadata = &ant.MetadataParam{UserID: ant.String("u1")}
//	}))
//
// Per-request (via Request.Extra):
//
//	stream, _ := llm.Generate(ctx, &niro.Request{
//	    Messages: msgs,
//	    Extra: anthropic.RequestHook(func(p *ant.MessageNewParams) { ... }),
//	})
type RequestHook func(params *ant.MessageNewParams)

// Provider implements niro.Provider using the official Anthropic SDK.
type Provider struct {
	client ant.Client
	model  string
	hooks  []RequestHook
}

var _ niro.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*providerConfig)

type providerConfig struct {
	opts  []option.RequestOption
	model string
	hooks []RequestHook
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

// WithRequestHook registers a function called with the raw SDK params
// before each request. Multiple hooks are called in registration order.
func WithRequestHook(fn RequestHook) Option {
	return func(c *providerConfig) {
		c.hooks = append(c.hooks, fn)
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
		hooks:  cfg.hooks,
	}
}

// NewFromClient creates a provider from an existing Anthropic SDK client.
func NewFromClient(client ant.Client, model string) *Provider {
	return &Provider{client: client, model: model}
}

// Client returns the underlying Anthropic SDK client.
// Use for direct SDK access when the niro abstraction is insufficient.
func (p *Provider) Client() ant.Client { return p.client }

// CacheCaps reports Anthropic cache support characteristics.
func (p *Provider) CacheCaps() niro.CacheCapabilities {
	return niro.CacheCapabilities{
		SupportsPrefix:       true,
		SupportsExplicitKeys: false,
		SupportsTTL:          true,
		SupportsBypass:       true,
	}
}

// Generate implements niro.Provider.
func (p *Provider) Generate(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
	if req == nil {
		return nil, fmt.Errorf("niro/anthropic: nil request")
	}
	if req.Options.ExperimentalReasoning {
		return nil, fmt.Errorf("niro/anthropic: experimental reasoning is not supported")
	}

	model := req.Model
	if model == "" {
		model = p.model
	}

	cacheHint, cacheEnabled := niro.GetCacheHint(ctx)
	cacheAttempted := cacheEnabled && cacheHint.Mode != niro.CacheBypass
	cacheRequire := cacheHint.Mode == niro.CacheRequire
	if cacheRequire {
		if err := validateRequireTTL(cacheHint.TTL); err != nil {
			return nil, niro.WrapError(niro.ErrCodeInvalidRequest, err.Error(), err).WithProvider("anthropic")
		}
	}

	params := p.buildParams(model, req, cacheHint, cacheAttempted)

	// Provider-level hooks
	for _, h := range p.hooks {
		h(&params)
	}
	// Per-request hook via Extra
	if hook, ok := req.Extra.(RequestHook); ok {
		hook(&params)
	}

	sdk := p.client.Messages.NewStreaming(ctx, params)

	stream, emitter := niro.NewStream(32)
	go p.consume(ctx, sdk, emitter, cacheAttempted, cacheRequire)
	return stream, nil
}

func (p *Provider) consume(
	ctx context.Context,
	sdk *ssestream.Stream[ant.MessageStreamEventUnion],
	out *niro.Emitter,
	cacheAttempted bool,
	cacheRequire bool,
) {
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
				if err := out.Emit(ctx, niro.TextFrame(delta.Text)); err != nil {
					return
				}
			}
		default:
			// Ignore other event types (MessageStart, ContentBlockStart, etc.)
		}
	}

	if err := sdk.Err(); err != nil {
		out.Error(fmt.Errorf("niro/anthropic: stream: %w", err))
		return
	}

	// Emit completed tool calls from accumulated message
	for _, block := range message.Content {
		if tu, ok := block.AsAny().(ant.ToolUseBlock); ok {
			tc := &niro.ToolCall{
				ID:   tu.ID,
				Name: tu.Name,
				Args: json.RawMessage(tu.Input),
			}
			if err := out.Emit(ctx, niro.ToolCallFrame(tc)); err != nil {
				return
			}
		}
	}

	// Emit usage
	cacheRead := int(message.Usage.CacheReadInputTokens)
	cacheWrite := int(message.Usage.CacheCreationInputTokens)
	usage := &niro.Usage{
		InputTokens:  int(message.Usage.InputTokens),
		OutputTokens: int(message.Usage.OutputTokens),
		TotalTokens:  int(message.Usage.InputTokens + message.Usage.OutputTokens),
	}
	niro.SetCacheUsageDetail(usage, cacheAttempted, cacheRead > 0, cacheWrite > 0, cacheRead, 0)
	if message.Usage.CacheReadInputTokens > 0 || message.Usage.CacheCreationInputTokens > 0 {
		usage.Detail = map[string]int{
			"cache_read_input_tokens":     int(message.Usage.CacheReadInputTokens),
			"cache_creation_input_tokens": int(message.Usage.CacheCreationInputTokens),
			niro.UsageCacheAttempted:      usage.Detail[niro.UsageCacheAttempted],
			niro.UsageCacheHit:            usage.Detail[niro.UsageCacheHit],
			niro.UsageCacheWrite:          usage.Detail[niro.UsageCacheWrite],
			niro.UsageCachedInputTokens:   usage.Detail[niro.UsageCachedInputTokens],
			niro.UsageCacheLatencySavedMS: usage.Detail[niro.UsageCacheLatencySavedMS],
		}
	}
	if cacheRequire && cacheAttempted && message.Usage.CacheReadInputTokens == 0 {
		out.Error(niro.NewError(niro.ErrCodeProviderError, "niro/anthropic: cache required but provider reported no cache read tokens").WithProvider("anthropic"))
		return
	}
	_ = out.Emit(ctx, niro.UsageFrame(usage))

	// Response metadata
	out.SetResponse(&niro.ResponseMeta{
		ID:           message.ID,
		Model:        string(message.Model),
		FinishReason: string(message.StopReason),
		Usage:        *usage,
	})
}

func (p *Provider) buildParams(model string, req *niro.Request, cacheHint niro.CacheHint, cacheAttempted bool) ant.MessageNewParams {
	params := ant.MessageNewParams{
		Model: ant.Model(model),
	}
	if cacheAttempted {
		cc := ant.NewCacheControlEphemeralParam()
		if ttl := mapAnthropicTTL(cacheHint.TTL); ttl != "" {
			cc.TTL = ttl
		}
		params.CacheControl = cc
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
		if msg.Role == niro.RoleSystem {
			// Anthropic: system messages go into params.System
			for _, part := range msg.Parts {
				if part.Kind == niro.KindText {
					params.System = append(params.System, ant.TextBlockParam{
						Text: ensureNonEmptyText(part.Text),
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
			_ = niro.JSONUnmarshal(tool.Parameters, &props)
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

func validateRequireTTL(ttl time.Duration) error {
	if ttl == 0 || ttl == 5*time.Minute || ttl == time.Hour {
		return nil
	}
	return fmt.Errorf("niro/anthropic: cache require supports TTL 5m or 1h only")
}

func mapAnthropicTTL(ttl time.Duration) ant.CacheControlEphemeralTTL {
	if ttl >= time.Hour {
		return ant.CacheControlEphemeralTTLTTL1h
	}
	if ttl >= 5*time.Minute {
		return ant.CacheControlEphemeralTTLTTL5m
	}
	return ""
}

func convertMessage(msg niro.Message) ant.MessageParam {
	switch msg.Role {
	case niro.RoleUser:
		var blocks []ant.ContentBlockParamUnion
		for _, p := range msg.Parts {
			switch p.Kind {
			case niro.KindText:
				blocks = append(blocks, ant.NewTextBlock(ensureNonEmptyText(p.Text)))
			case niro.KindImage:
				if len(p.Data) > 0 {
					b64 := base64.StdEncoding.EncodeToString(p.Data)
					blocks = append(blocks, ant.NewImageBlockBase64(p.Mime, b64))
				}
			}
		}
		return ant.NewUserMessage(blocks...)

	case niro.RoleAssistant:
		var blocks []ant.ContentBlockParamUnion
		for _, p := range msg.Parts {
			switch p.Kind {
			case niro.KindText:
				blocks = append(blocks, ant.NewTextBlock(ensureNonEmptyText(p.Text)))
			case niro.KindToolCall:
				if p.Tool != nil {
					var input any
					_ = niro.JSONUnmarshal(p.Tool.Args, &input)
					blocks = append(blocks, ant.NewToolUseBlock(p.Tool.ID, input, p.Tool.Name))
				}
			}
		}
		return ant.NewAssistantMessage(blocks...)

	case niro.RoleTool:
		// Anthropic: tool results are in user turns
		var blocks []ant.ContentBlockParamUnion
		for _, p := range msg.Parts {
			if p.Kind == niro.KindToolResult && p.Result != nil {
				blocks = append(blocks, ant.NewToolResultBlock(
					p.Result.CallID,
					p.Result.Content,
					p.Result.IsError,
				))
			}
		}
		return ant.NewUserMessage(blocks...)

	default:
		return ant.NewUserMessage(ant.NewTextBlock(ensureNonEmptyText(extractText(msg))))
	}
}

// ensureNonEmptyText returns s, or " " if s is empty, for API compatibility (e.g. handoff with no classifier text).
func ensureNonEmptyText(s string) string {
	if s == "" {
		return " "
	}
	return s
}

func extractText(msg niro.Message) string {
	for _, p := range msg.Parts {
		if p.Kind == niro.KindText {
			return p.Text
		}
	}
	return ""
}
