// Package openai implements a Niro Provider backed by the official
// OpenAI Go SDK (github.com/openai/openai-go).
//
// This is an opt-in provider module. Import it only when you need
// OpenAI (or Azure OpenAI, or any OpenAI-compatible endpoint):
//
//	go get github.com/alexedtionweb/niro-stream/provider/openai
//
// # SDK Access
//
// Use [Provider.Client] to access the underlying SDK client directly
// for operations not covered by the niro abstraction.
//
// Use [WithRequestHook] (provider-level) or pass a [RequestHook] as
// Request.Extra (per-request) to modify the raw SDK params before
// each API call.
//
// # Usage
//
//	llm := openai.New(os.Getenv("OPENAI_API_KEY"))
//	stream, err := llm.Generate(ctx, &niro.Request{
//	    Model: "gpt-4o",
//	    Messages: []niro.Message{niro.UserText("Hello")},
//	})
package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	oai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/ssestream"
	"github.com/openai/openai-go/shared"

	"github.com/alexedtionweb/niro-stream"
)

// RequestHook allows modifying the raw SDK params before the request is sent.
// Use this to set SDK-specific parameters not exposed by niro.Request.
//
// Provider-level (every request):
//
//	openai.New(key, openai.WithRequestHook(func(p *oai.ChatCompletionNewParams) {
//	    p.LogProbs = oai.Bool(true)
//	}))
//
// Per-request (via Request.Extra):
//
//	stream, _ := llm.Generate(ctx, &niro.Request{
//	    Messages: msgs,
//	    Extra: openai.RequestHook(func(p *oai.ChatCompletionNewParams) {
//	        p.LogProbs = oai.Bool(true)
//	    }),
//	})
type RequestHook func(params *oai.ChatCompletionNewParams)

// Provider implements niro.Provider using the official OpenAI SDK.
type Provider struct {
	client oai.Client
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

// WithBaseURL overrides the API base URL.
// Use for Azure OpenAI, Ollama, vLLM, or any compatible endpoint.
func WithBaseURL(url string) Option {
	return func(c *providerConfig) {
		c.opts = append(c.opts, option.WithBaseURL(url))
	}
}

// WithModel sets the default model.
func WithModel(model string) Option {
	return func(c *providerConfig) { c.model = model }
}

// WithRequestOption appends a raw SDK request option.
// Use for custom headers, timeouts, middleware, etc.
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

// New creates an OpenAI provider.
func New(apiKey string, opts ...Option) *Provider {
	cfg := &providerConfig{model: "gpt-4o"}
	for _, o := range opts {
		o(cfg)
	}
	clientOpts := append([]option.RequestOption{option.WithAPIKey(apiKey)}, cfg.opts...)
	return &Provider{
		client: oai.NewClient(clientOpts...),
		model:  cfg.model,
		hooks:  cfg.hooks,
	}
}

// NewFromClient creates a provider from an existing OpenAI SDK client.
// Use when you need full control over client construction.
func NewFromClient(client oai.Client, model string) *Provider {
	return &Provider{client: client, model: model}
}

// Client returns the underlying OpenAI SDK client.
// Use for direct SDK access when the niro abstraction is insufficient.
func (p *Provider) Client() oai.Client { return p.client }

// CacheCaps reports OpenAI cache support characteristics.
func (p *Provider) CacheCaps() niro.CacheCapabilities {
	return niro.CacheCapabilities{
		SupportsPrefix:       true,  // OpenAI prompt caching is automatic for shared prefixes.
		SupportsExplicitKeys: false, // No explicit cache-key API for chat completions.
		SupportsTTL:          false, // TTL is provider-managed.
		SupportsBypass:       true,  // Bypass is respected by omitting cache hints.
	}
}

// Generate implements niro.Provider.
func (p *Provider) Generate(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
	if req == nil {
		return nil, fmt.Errorf("niro/openai: nil request")
	}
	if req.Options.ExperimentalReasoning {
		return nil, fmt.Errorf("niro/openai: experimental reasoning is not supported")
	}

	model := req.Model
	if model == "" {
		model = p.model
	}

	cacheHint, cacheEnabled := niro.GetCacheHint(ctx)
	cacheAttempted := cacheEnabled && cacheHint.Mode != niro.CacheBypass

	params := buildParams(model, req)

	// Provider-level hooks
	for _, h := range p.hooks {
		h(&params)
	}
	// Per-request hook via Extra
	if hook, ok := req.Extra.(RequestHook); ok {
		hook(&params)
	}

	sdk := p.client.Chat.Completions.NewStreaming(ctx, params)

	stream, emitter := niro.NewStream(niro.DefaultStreamBuffer)
	go consume(ctx, sdk, emitter, cacheAttempted, cacheHint.Mode == niro.CacheRequire)
	return stream, nil
}

// consume reads from the SDK stream and emits niro Frames.
func consume(
	ctx context.Context,
	sdk *ssestream.Stream[oai.ChatCompletionChunk],
	out *niro.Emitter,
	cacheAttempted bool,
	cacheRequire bool,
) {
	defer out.Close()
	defer sdk.Close()

	acc := oai.ChatCompletionAccumulator{}
	cachedInputTokens := 0

	for sdk.Next() {
		chunk := sdk.Current()
		acc.AddChunk(chunk)

		// Text token delta
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			if delta.Content != "" {
				if err := out.Emit(ctx, niro.TextFrame(delta.Content)); err != nil {
					return
				}
			}
		}

		// Complete tool call detection via accumulator
		if tool, ok := acc.JustFinishedToolCall(); ok {
			tc := &niro.ToolCall{
				ID:   tool.Id,
				Name: tool.Name,
				Args: json.RawMessage(tool.Arguments),
			}
			if err := out.Emit(ctx, niro.ToolCallFrame(tc)); err != nil {
				return
			}
		}

		// Token usage (present in the final chunk when StreamOptions.IncludeUsage is set)
		if chunk.Usage.TotalTokens > 0 {
			cachedInputTokens = int(chunk.Usage.PromptTokensDetails.CachedTokens)
			usage := &niro.Usage{
				InputTokens:  int(chunk.Usage.PromptTokens),
				OutputTokens: int(chunk.Usage.CompletionTokens),
				TotalTokens:  int(chunk.Usage.TotalTokens),
			}
			niro.SetCacheUsageDetail(usage, cacheAttempted, cachedInputTokens > 0, false, cachedInputTokens, 0)
			if cacheRequire && cacheAttempted && chunk.Usage.PromptTokensDetails.RawJSON() != "" && chunk.Usage.PromptTokens > 0 && cachedInputTokens == 0 {
				out.Error(niro.NewError(niro.ErrCodeProviderError, "niro/openai: cache required but provider reported no cached input tokens").WithProvider("openai"))
				return
			}
			if err := out.Emit(ctx, niro.UsageFrame(usage)); err != nil {
				return
			}
		}
	}

	if err := sdk.Err(); err != nil {
		out.Error(fmt.Errorf("niro/openai: stream: %w", err))
		return
	}

	// Set response metadata from accumulated completion
	completion := acc.ChatCompletion
	cachedInputTokens = int(completion.Usage.PromptTokensDetails.CachedTokens)
	if cacheRequire && cacheAttempted && completion.Usage.PromptTokensDetails.RawJSON() != "" && completion.Usage.PromptTokens > 0 && cachedInputTokens == 0 {
		out.Error(niro.NewError(niro.ErrCodeProviderError, "niro/openai: cache required but provider reported no cached input tokens").WithProvider("openai"))
		return
	}
	finalUsage := niro.Usage{
		InputTokens:  int(completion.Usage.PromptTokens),
		OutputTokens: int(completion.Usage.CompletionTokens),
		TotalTokens:  int(completion.Usage.TotalTokens),
	}
	niro.SetCacheUsageDetail(&finalUsage, cacheAttempted, cachedInputTokens > 0, false, cachedInputTokens, 0)

	meta := &niro.ResponseMeta{
		ID:    completion.ID,
		Model: completion.Model,
		Usage: finalUsage,
	}
	if len(completion.Choices) > 0 {
		meta.FinishReason = string(completion.Choices[0].FinishReason)
	}
	out.SetResponse(meta)
}

// --- Request building ---

func buildParams(model string, req *niro.Request) oai.ChatCompletionNewParams {
	params := oai.ChatCompletionNewParams{
		Model: model,
		StreamOptions: oai.ChatCompletionStreamOptionsParam{
			IncludeUsage: oai.Bool(true),
		},
	}

	// Messages
	msgs := req.EffectiveMessages()
	for _, msg := range msgs {
		// RoleTool messages may carry multiple tool results (one per parallel tool
		// call). OpenAI expects one tool message per result, so we expand them.
		params.Messages = append(params.Messages, convertMessages(msg)...)
	}

	// Options
	if req.Options.MaxTokens > 0 {
		params.MaxCompletionTokens = oai.Int(int64(req.Options.MaxTokens))
	}
	if req.Options.Temperature != nil {
		params.Temperature = oai.Float(*req.Options.Temperature)
	}
	if req.Options.TopP != nil {
		params.TopP = oai.Float(*req.Options.TopP)
	}
	if req.Options.FrequencyPenalty != nil {
		params.FrequencyPenalty = oai.Float(*req.Options.FrequencyPenalty)
	}
	if req.Options.PresencePenalty != nil {
		params.PresencePenalty = oai.Float(*req.Options.PresencePenalty)
	}
	if len(req.Options.Stop) > 0 {
		params.Stop = oai.ChatCompletionNewParamsStopUnion{
			OfChatCompletionNewsStopArray: req.Options.Stop,
		}
	}

	// Tools
	for _, tool := range req.Tools {
		params.Tools = append(params.Tools, oai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        tool.Name,
				Description: oai.String(tool.Description),
				Parameters:  shared.FunctionParameters(rawToMap(tool.Parameters)),
			},
		})
	}

	// Tool choice
	if req.ToolChoice != "" {
		switch req.ToolChoice {
		case niro.ToolChoiceAuto:
			params.ToolChoice = oai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: oai.String(string(oai.ChatCompletionToolChoiceOptionAutoAuto)),
			}
		case niro.ToolChoiceNone:
			params.ToolChoice = oai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: oai.String(string(oai.ChatCompletionToolChoiceOptionAutoNone)),
			}
		case niro.ToolChoiceRequired:
			params.ToolChoice = oai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: oai.String(string(oai.ChatCompletionToolChoiceOptionAutoRequired)),
			}
		default:
			// Check for func:name pattern
			if name := strings.TrimPrefix(string(req.ToolChoice), niro.ToolChoiceFuncPrefix); name != "" && name != string(req.ToolChoice) {
				params.ToolChoice = oai.ChatCompletionToolChoiceOptionParamOfChatCompletionNamedToolChoice(
					oai.ChatCompletionNamedToolChoiceFunctionParam{Name: name},
				)
			}
		}
	}

	return params
}

// convertMessages converts a niro.Message to one or more OpenAI message params.
// RoleTool messages may contain multiple tool results (from parallel tool calls
// in one round). OpenAI requires one tool message per result, so this function
// expands them. All other roles produce exactly one param.
func convertMessages(msg niro.Message) []oai.ChatCompletionMessageParamUnion {
	if msg.Role == niro.RoleTool {
		var out []oai.ChatCompletionMessageParamUnion
		for _, p := range msg.Parts {
			if p.Kind == niro.KindToolResult && p.Result != nil {
				out = append(out, oai.ToolMessage(p.Result.Content, p.Result.CallID))
			}
		}
		if len(out) == 0 {
			// Fallback: malformed message with no tool result parts.
			out = append(out, oai.ToolMessage(extractText(msg), ""))
		}
		return out
	}
	return []oai.ChatCompletionMessageParamUnion{convertMessage(msg)}
}

func convertMessage(msg niro.Message) oai.ChatCompletionMessageParamUnion {
	switch msg.Role {
	case niro.RoleSystem:
		return oai.SystemMessage(ensureNonEmptyText(extractText(msg)))

	case niro.RoleUser:
		// Single text part — use simple constructor
		if len(msg.Parts) == 1 && msg.Parts[0].Kind == niro.KindText {
			return oai.UserMessage(ensureNonEmptyText(msg.Parts[0].Text))
		}
		// Multimodal — build content parts
		var parts []oai.ChatCompletionContentPartUnionParam
		for _, p := range msg.Parts {
			switch p.Kind {
			case niro.KindText:
				parts = append(parts, oai.TextContentPart(ensureNonEmptyText(p.Text)))
			case niro.KindImage:
				url := p.URL
				if url == "" && len(p.Data) > 0 {
					url = dataURI(p.Data, p.Mime)
				}
				parts = append(parts, oai.ImageContentPart(oai.ChatCompletionContentPartImageImageURLParam{
					URL: url,
				}))
			}
		}
		return oai.UserMessage(parts)

	case niro.RoleAssistant:
		text := ensureNonEmptyText(extractText(msg))
		m := oai.AssistantMessage(text)
		// Attach tool calls if present
		if m.OfAssistant != nil {
			for _, p := range msg.Parts {
				if p.Kind == niro.KindToolCall && p.Tool != nil {
					tc := oai.ChatCompletionMessageToolCallParam{
						ID: p.Tool.ID,
						Function: oai.ChatCompletionMessageToolCallFunctionParam{
							Name:      p.Tool.Name,
							Arguments: string(p.Tool.Args),
						},
					}
					m.OfAssistant.ToolCalls = append(m.OfAssistant.ToolCalls, tc)
				}
			}
		}
		return m

	case niro.RoleTool:
		if len(msg.Parts) > 0 && msg.Parts[0].Result != nil {
			r := msg.Parts[0].Result
			return oai.ToolMessage(ensureNonEmptyText(r.Content), r.CallID)
		}
		return oai.ToolMessage(ensureNonEmptyText(extractText(msg)), "")

	default:
		return oai.UserMessage(ensureNonEmptyText(extractText(msg)))
	}
}

func extractText(msg niro.Message) string {
	for _, p := range msg.Parts {
		if p.Kind == niro.KindText {
			return p.Text
		}
	}
	return ""
}

// ensureNonEmptyText returns s, or " " if s is empty, so APIs that reject empty content (e.g. handoff with no classifier text) stay compatible.
func ensureNonEmptyText(s string) string {
	if s == "" {
		return " "
	}
	return s
}

func dataURI(data []byte, mime string) string {
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + encodeBase64(data)
}

func rawToMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	_ = niro.JSONUnmarshal(raw, &m)
	return m
}
