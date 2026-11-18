// Package openrouter implements a Niro Provider backed by the official
// OpenRouter Go SDK (github.com/OpenRouterTeam/go-sdk).
//
// This is an opt-in provider module. Import it only when you need
// OpenRouter (400+ models via one OpenAI-compatible API):
//
//	go get github.com/alexedtionweb/niro-stream/provider/openrouter
//
// # SDK Access
//
// Use [Provider.Client] to access the underlying SDK client directly
// for operations not covered by the niro abstraction (models list,
// credits, analytics, provider routing, etc.).
//
// Use [WithRequestHook] (provider-level) or pass a [RequestHook] as
// Request.Extra (per-request) to modify the raw SDK ChatRequest before
// each API call.
//
// # Usage
//
//	llm := openrouter.New(os.Getenv("OPENROUTER_API_KEY"),
//	    openrouter.WithHTTPReferer("https://myapp.example"),
//	    openrouter.WithXTitle("My App"),
//	)
//	stream, err := llm.Generate(ctx, &niro.Request{
//	    Model: "openai/gpt-4o",
//	    Messages: []niro.Message{niro.UserText("Hello")},
//	})
//
// # Image / video / decisions
//
// The same Provider also implements [niro.ImageProvider], [niro.VideoProvider],
// and [niro.DecisionProvider]:
//
//	img, err := llm.GenerateImage(ctx, &niro.ImageRequest{
//	    Model:  "recraft/recraft-v4.1-flash-v2:free",
//	    Prompt: "A serene mountain landscape at sunset",
//	})
//	vid, err := llm.GenerateVideo(ctx, &niro.VideoRequest{
//	    Model:  "google/veo-3",
//	    Prompt: "A cat walking through a neon city",
//	})
//	dec, err := llm.Decide(ctx, &niro.DecisionRequest{
//	    Model: "typesafe/jev-1.13",
//	    State: "Checkout shows a blank screen after Pay.",
//	    Questions: map[string]niro.DecisionQuestion{
//	        "is_bug": niro.Noul("Is this a software defect?", "broken behavior", "feature request"),
//	    },
//	})
package openrouter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	orsdk "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
	"github.com/OpenRouterTeam/go-sdk/types/stream"

	"github.com/alexedtionweb/niro-stream"
)

const defaultModel = "openai/gpt-4o"

// RequestHook allows modifying the raw SDK ChatRequest before it is sent.
//
// Provider-level (every request):
//
//	openrouter.New(key, openrouter.WithRequestHook(func(r *components.ChatRequest) {
//	    r.Provider = optionalnullable.From(&components.ProviderPreferences{
//	        // ...
//	    })
//	}))
//
// Per-request (via Request.Extra):
//
//	stream, _ := llm.Generate(ctx, &niro.Request{
//	    Messages: msgs,
//	    Extra: openrouter.RequestHook(func(r *components.ChatRequest) {
//	        r.User = orsdk.Pointer("user-123")
//	    }),
//	})
type RequestHook func(req *components.ChatRequest)

// Provider implements [niro.Provider], [niro.ImageProvider],
// [niro.VideoProvider], and [niro.DecisionProvider] using the official
// OpenRouter SDK.
type Provider struct {
	client        *orsdk.OpenRouter
	model         string
	imageModel    string
	videoModel    string
	decisionModel string
	serverURL     string
	hooks         []RequestHook
	imageHooks    []ImageRequestHook
	videoHooks    []VideoRequestHook
	decisionHooks []DecisionRequestHook
}

var (
	_ niro.Provider         = (*Provider)(nil)
	_ niro.ImageProvider    = (*Provider)(nil)
	_ niro.VideoProvider    = (*Provider)(nil)
	_ niro.DecisionProvider = (*Provider)(nil)
)

// Option configures a Provider.
type Option func(*providerConfig)

type providerConfig struct {
	sdkOpts       []orsdk.SDKOption
	model         string
	imageModel    string
	videoModel    string
	decisionModel string
	serverURL     string
	hooks         []RequestHook
	imageHooks    []ImageRequestHook
	videoHooks    []VideoRequestHook
	decisionHooks []DecisionRequestHook
	httpClient    orsdk.HTTPClient
	referer       string
	title         string
}

// WithModel sets the default chat model (OpenRouter slug, e.g. "anthropic/claude-sonnet-4.5").
func WithModel(model string) Option {
	return func(c *providerConfig) { c.model = model }
}

// WithImageModel sets the default image generation model.
func WithImageModel(model string) Option {
	return func(c *providerConfig) { c.imageModel = model }
}

// WithVideoModel sets the default video generation model.
func WithVideoModel(model string) Option {
	return func(c *providerConfig) { c.videoModel = model }
}

// WithDecisionModel sets the default Decisions model.
func WithDecisionModel(model string) Option {
	return func(c *providerConfig) { c.decisionModel = model }
}

// WithHTTPReferer sets the HTTP-Referer header OpenRouter uses for app attribution.
func WithHTTPReferer(referer string) Option {
	return func(c *providerConfig) { c.referer = referer }
}

// WithXTitle sets the X-Title header OpenRouter uses for app attribution.
func WithXTitle(title string) Option {
	return func(c *providerConfig) { c.title = title }
}

// WithServerURL overrides the API base URL (default https://openrouter.ai/api/v1).
// Decisions (Alpha) are host-rooted at /api/alpha/decisions; a trailing /api/v1
// or /v1 is stripped for those calls.
func WithServerURL(url string) Option {
	return func(c *providerConfig) {
		c.serverURL = url
		c.sdkOpts = append(c.sdkOpts, orsdk.WithServerURL(url))
	}
}

// WithClient sets a custom HTTP client for the SDK.
func WithClient(client orsdk.HTTPClient) Option {
	return func(c *providerConfig) { c.httpClient = client }
}

// WithSDKOption appends a raw OpenRouter SDK option.
func WithSDKOption(opt orsdk.SDKOption) Option {
	return func(c *providerConfig) {
		c.sdkOpts = append(c.sdkOpts, opt)
	}
}

// WithRequestHook registers a function called with the raw SDK ChatRequest
// before each request. Multiple hooks are called in registration order.
func WithRequestHook(fn RequestHook) Option {
	return func(c *providerConfig) {
		c.hooks = append(c.hooks, fn)
	}
}

// WithImageRequestHook registers a hook for image generation requests.
func WithImageRequestHook(fn ImageRequestHook) Option {
	return func(c *providerConfig) {
		c.imageHooks = append(c.imageHooks, fn)
	}
}

// WithVideoRequestHook registers a hook for video generation requests.
func WithVideoRequestHook(fn VideoRequestHook) Option {
	return func(c *providerConfig) {
		c.videoHooks = append(c.videoHooks, fn)
	}
}

// WithDecisionRequestHook registers a hook for Decisions requests.
func WithDecisionRequestHook(fn DecisionRequestHook) Option {
	return func(c *providerConfig) {
		c.decisionHooks = append(c.decisionHooks, fn)
	}
}

// headerClient injects OpenRouter attribution headers on every request.
// The upstream SDK stores HTTP-Referer / X-Title as globals but currently
// does not apply them on Chat.Send, so we set them on the wire ourselves.
type headerClient struct {
	base    orsdk.HTTPClient
	referer string
	title   string
}

func (c *headerClient) Do(req *http.Request) (*http.Response, error) {
	if c.referer != "" && req.Header.Get("HTTP-Referer") == "" {
		req.Header.Set("HTTP-Referer", c.referer)
	}
	if c.title != "" && req.Header.Get("X-Title") == "" {
		req.Header.Set("X-Title", c.title)
	}
	return c.base.Do(req)
}

// New creates an OpenRouter provider.
func New(apiKey string, opts ...Option) *Provider {
	cfg := &providerConfig{
		model:         defaultModel,
		imageModel:    defaultImageModel,
		videoModel:    defaultVideoModel,
		decisionModel: defaultDecisionModel,
	}
	for _, o := range opts {
		o(cfg)
	}

	sdkOpts := append([]orsdk.SDKOption{orsdk.WithSecurity(apiKey)}, cfg.sdkOpts...)

	base := cfg.httpClient
	if base == nil {
		base = http.DefaultClient
	}
	if cfg.referer != "" || cfg.title != "" {
		base = &headerClient{base: base, referer: cfg.referer, title: cfg.title}
		if cfg.referer != "" {
			sdkOpts = append(sdkOpts, orsdk.WithHTTPReferer(cfg.referer))
		}
		if cfg.title != "" {
			sdkOpts = append(sdkOpts, orsdk.WithXTitle(cfg.title))
		}
	}
	sdkOpts = append(sdkOpts, orsdk.WithClient(base))

	return &Provider{
		client:        orsdk.New(sdkOpts...),
		model:         cfg.model,
		imageModel:    cfg.imageModel,
		videoModel:    cfg.videoModel,
		decisionModel: cfg.decisionModel,
		serverURL:     cfg.serverURL,
		hooks:         cfg.hooks,
		imageHooks:    cfg.imageHooks,
		videoHooks:    cfg.videoHooks,
		decisionHooks: cfg.decisionHooks,
	}
}

// NewFromClient creates a provider from an existing OpenRouter SDK client.
func NewFromClient(client *orsdk.OpenRouter, model string) *Provider {
	if model == "" {
		model = defaultModel
	}
	return &Provider{
		client:        client,
		model:         model,
		imageModel:    defaultImageModel,
		videoModel:    defaultVideoModel,
		decisionModel: defaultDecisionModel,
	}
}

// Client returns the underlying OpenRouter SDK client.
func (p *Provider) Client() *orsdk.OpenRouter { return p.client }

// CacheCaps reports OpenRouter cache support characteristics.
// Caching behavior depends on the upstream model/provider; OpenRouter
// forwards cache hints where supported.
func (p *Provider) CacheCaps() niro.CacheCapabilities {
	return niro.CacheCapabilities{
		SupportsPrefix:       true,
		SupportsExplicitKeys: false,
		SupportsTTL:          false,
		SupportsBypass:       true,
	}
}

// Generate implements niro.Provider.
func (p *Provider) Generate(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
	if req == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "nil request")
	}
	if req.Options.ExperimentalReasoning {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "experimental reasoning is not supported")
	}

	model := req.Model
	if model == "" {
		model = p.model
	}

	cacheHint, cacheEnabled := niro.GetCacheHint(ctx)
	cacheAttempted := cacheEnabled && cacheHint.Mode != niro.CacheBypass

	params := buildParams(model, req)

	for _, h := range p.hooks {
		h(&params)
	}
	if hook, ok := req.Extra.(RequestHook); ok {
		hook(&params)
	}

	res, err := p.client.Chat.Send(ctx, params, nil)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "openrouter", err).WithProvider("openrouter")
	}
	if res == nil || res.EventStream == nil {
		return nil, niro.NewError(niro.ErrCodeProviderError, "openrouter: expected streaming response").WithProvider("openrouter")
	}

	stream, emitter := niro.NewStream(niro.DefaultStreamBuffer)
	go consume(ctx, res.EventStream, emitter, cacheAttempted, cacheHint.Mode == niro.CacheRequire)
	return stream, nil
}

func consume(
	ctx context.Context,
	sdk *stream.EventStream[components.ChatStreamingResponse],
	out *niro.Emitter,
	cacheAttempted bool,
	cacheRequire bool,
) {
	defer out.Close()
	defer sdk.Close()

	type toolBuf struct {
		id, name string
		args     strings.Builder
	}
	tools := map[int64]*toolBuf{}
	emittedTools := false

	var (
		respID, respModel, finishReason string
		finalUsage                      niro.Usage
		cachedInputTokens               int
	)

	flushTools := func() bool {
		if emittedTools || len(tools) == 0 {
			return true
		}
		emittedTools = true
		var maxIdx int64 = -1
		for i := range tools {
			if i > maxIdx {
				maxIdx = i
			}
		}
		for i := int64(0); i <= maxIdx; i++ {
			tb, ok := tools[i]
			if !ok {
				continue
			}
			args := tb.args.String()
			if args == "" {
				args = "{}"
			}
			tc := &niro.ToolCall{
				ID:   tb.id,
				Name: tb.name,
				Args: json.RawMessage(args),
			}
			if err := out.Emit(ctx, niro.ToolCallFrame(tc)); err != nil {
				return false
			}
		}
		return true
	}

	for sdk.Next() {
		ev := sdk.Value()
		if ev == nil {
			continue
		}
		chunk := ev.Data

		if chunk.Error != nil {
			out.Error(niro.NewError(niro.ErrCodeProviderError, "openrouter: "+chunk.Error.Message).WithProvider("openrouter"))
			return
		}
		if chunk.ID != "" {
			respID = chunk.ID
		}
		if chunk.Model != "" {
			respModel = chunk.Model
		}

		for _, choice := range chunk.Choices {
			delta := choice.Delta

			if text, ok := delta.Content.Get(); ok && text != nil && *text != "" {
				if err := out.Emit(ctx, niro.TextFrame(*text)); err != nil {
					return
				}
			}

			if reasoning, ok := delta.Reasoning.Get(); ok && reasoning != nil && *reasoning != "" {
				if err := out.Emit(ctx, niro.CustomFrame(&niro.ExperimentalFrame{
					Type: niro.CustomThinking,
					Data: *reasoning,
				})); err != nil {
					return
				}
			}

			for _, tc := range delta.ToolCalls {
				tb := tools[tc.Index]
				if tb == nil {
					tb = &toolBuf{}
					tools[tc.Index] = tb
				}
				if tc.ID != nil && *tc.ID != "" {
					tb.id = *tc.ID
				}
				if tc.Function != nil {
					if tc.Function.Name != nil && *tc.Function.Name != "" {
						tb.name = *tc.Function.Name
					}
					if tc.Function.Arguments != nil {
						tb.args.WriteString(*tc.Function.Arguments)
					}
				}
			}

			if choice.FinishReason != nil {
				finishReason = string(*choice.FinishReason)
				if *choice.FinishReason == components.ChatFinishReasonEnumToolCalls {
					if !flushTools() {
						return
					}
				}
			}
		}

		if chunk.Usage != nil {
			u := chunk.Usage
			cachedInputTokens = 0
			if details, ok := u.PromptTokensDetails.Get(); ok && details != nil && details.CachedTokens != nil {
				cachedInputTokens = int(*details.CachedTokens)
			}
			usage := &niro.Usage{
				InputTokens:  int(u.PromptTokens),
				OutputTokens: int(u.CompletionTokens),
				TotalTokens:  int(u.TotalTokens),
			}
			if details, ok := u.CompletionTokensDetails.Get(); ok && details != nil {
				if rt, ok := details.ReasoningTokens.Get(); ok && rt != nil {
					if usage.Detail == nil {
						usage.Detail = map[string]int{}
					}
					usage.Detail[niro.UsageReasoningTokens] = int(*rt)
				}
			}
			niro.SetCacheUsageDetail(usage, cacheAttempted, cachedInputTokens > 0, false, cachedInputTokens, 0)
			if cacheRequire && cacheAttempted && u.PromptTokens > 0 && cachedInputTokens == 0 {
				if _, hasDetails := u.PromptTokensDetails.Get(); hasDetails {
					out.Error(niro.NewError(niro.ErrCodeProviderError, "niro/openrouter: cache required but provider reported no cached input tokens").WithProvider("openrouter"))
					return
				}
			}
			finalUsage = *usage
			if err := out.Emit(ctx, niro.UsageFrame(usage)); err != nil {
				return
			}
		}
	}

	if err := sdk.Err(); err != nil {
		out.Error(niro.WrapError(niro.ErrCodeStreamError, "stream", err))
		return
	}

	if !flushTools() {
		return
	}

	if cacheRequire && cacheAttempted && finalUsage.InputTokens > 0 && cachedInputTokens == 0 {
		out.Error(niro.NewError(niro.ErrCodeProviderError, "niro/openrouter: cache required but provider reported no cached input tokens").WithProvider("openrouter"))
		return
	}

	out.SetResponse(&niro.ResponseMeta{
		ID:           respID,
		Model:        respModel,
		Usage:        finalUsage,
		FinishReason: finishReason,
	})
}

func buildParams(model string, req *niro.Request) components.ChatRequest {
	params := components.ChatRequest{
		Model:  orsdk.Pointer(model),
		Stream: orsdk.Pointer(true),
		StreamOptions: optionalnullable.From(&components.ChatStreamOptions{
			IncludeUsage: orsdk.Pointer(true),
		}),
	}

	for _, msg := range req.EffectiveMessages() {
		params.Messages = append(params.Messages, convertMessages(msg)...)
	}

	if req.Options.MaxTokens > 0 {
		params.MaxCompletionTokens = optionalnullable.From(orsdk.Pointer(int64(req.Options.MaxTokens)))
	}
	if req.Options.Temperature != nil {
		params.Temperature = optionalnullable.From(orsdk.Pointer(*req.Options.Temperature))
	}
	if req.Options.TopP != nil {
		params.TopP = optionalnullable.From(orsdk.Pointer(*req.Options.TopP))
	}
	if req.Options.TopK != nil {
		params.TopK = optionalnullable.From(orsdk.Pointer(int64(*req.Options.TopK)))
	}
	if req.Options.FrequencyPenalty != nil {
		params.FrequencyPenalty = optionalnullable.From(orsdk.Pointer(*req.Options.FrequencyPenalty))
	}
	if req.Options.PresencePenalty != nil {
		params.PresencePenalty = optionalnullable.From(orsdk.Pointer(*req.Options.PresencePenalty))
	}
	if len(req.Options.Stop) > 0 {
		stop := components.CreateChatRequestStopArrayOfStr(req.Options.Stop)
		params.Stop = optionalnullable.From(&stop)
	}

	switch req.ResponseFormat {
	case "json":
		rf := components.CreateResponseFormatJSONObject(components.ChatFormatJSONObjectConfig{
			Type: components.ChatFormatJSONObjectConfigTypeJSONObject,
		})
		params.ResponseFormat = &rf
	case "json_schema":
		schemaName, schemaObj := splitJSONSchemaParams(req.ResponseSchema)
		if schemaObj != nil {
			rf := components.CreateResponseFormatJSONSchema(components.ChatFormatJSONSchemaConfig{
				Type: components.ChatFormatJSONSchemaConfigTypeJSONSchema,
				JSONSchema: components.ChatJSONSchemaConfig{
					Name:   schemaName,
					Schema: schemaObj,
					Strict: optionalnullable.From(orsdk.Pointer(true)),
				},
			})
			params.ResponseFormat = &rf
		}
	}

	for _, tool := range req.Tools {
		params.Tools = append(params.Tools, components.CreateChatFunctionToolChatFunctionToolFunction(
			components.ChatFunctionToolFunction{
				Type: components.ChatFunctionToolTypeFunction,
				Function: components.ChatFunctionToolFunctionFunction{
					Name:        tool.Name,
					Description: orsdk.Pointer(tool.Description),
					Parameters:  rawToMap(tool.Parameters),
				},
			},
		))
	}

	if req.ToolChoice != "" {
		switch req.ToolChoice {
		case niro.ToolChoiceAuto:
			auto := components.ChatToolChoiceAutoAuto
			tc := components.CreateChatToolChoiceChatToolChoiceAuto(auto)
			params.ToolChoice = &tc
		case niro.ToolChoiceNone:
			none := components.ChatToolChoiceNoneNone
			tc := components.CreateChatToolChoiceChatToolChoiceNone(none)
			params.ToolChoice = &tc
		case niro.ToolChoiceRequired:
			reqd := components.ChatToolChoiceRequiredRequired
			tc := components.CreateChatToolChoiceChatToolChoiceRequired(reqd)
			params.ToolChoice = &tc
		default:
			if name := strings.TrimPrefix(string(req.ToolChoice), niro.ToolChoiceFuncPrefix); name != "" && name != string(req.ToolChoice) {
				tc := components.CreateChatToolChoiceChatNamedToolChoice(components.ChatNamedToolChoice{
					Type: components.ChatNamedToolChoiceTypeFunction,
					Function: components.ChatNamedToolChoiceFunction{
						Name: name,
					},
				})
				params.ToolChoice = &tc
			}
		}
	}

	return params
}

func convertMessages(msg niro.Message) []components.ChatMessages {
	if msg.Role == niro.RoleTool {
		var out []components.ChatMessages
		for _, p := range msg.Parts {
			if p.Kind == niro.KindToolResult && p.Result != nil {
				out = append(out, components.CreateChatMessagesTool(components.ChatToolMessage{
					Role:       components.ChatToolMessageRoleTool,
					ToolCallID: p.Result.CallID,
					Content:    components.CreateChatToolMessageContentStr(ensureNonEmptyText(p.Result.Content)),
				}))
			}
		}
		if len(out) == 0 {
			out = append(out, components.CreateChatMessagesTool(components.ChatToolMessage{
				Role:       components.ChatToolMessageRoleTool,
				ToolCallID: "",
				Content:    components.CreateChatToolMessageContentStr(ensureNonEmptyText(extractText(msg))),
			}))
		}
		return out
	}
	return []components.ChatMessages{convertMessage(msg)}
}

func convertMessage(msg niro.Message) components.ChatMessages {
	switch msg.Role {
	case niro.RoleSystem:
		return components.CreateChatMessagesSystem(components.ChatSystemMessage{
			Role:    components.ChatSystemMessageRoleSystem,
			Content: components.CreateChatSystemMessageContentStr(ensureNonEmptyText(extractText(msg))),
		})

	case niro.RoleUser:
		if len(msg.Parts) == 1 && msg.Parts[0].Kind == niro.KindText {
			return components.CreateChatMessagesUser(components.ChatUserMessage{
				Role:    components.ChatUserMessageRoleUser,
				Content: components.CreateChatUserMessageContentStr(ensureNonEmptyText(msg.Parts[0].Text)),
			})
		}
		var parts []components.ChatContentItems
		for _, p := range msg.Parts {
			switch p.Kind {
			case niro.KindText:
				parts = append(parts, components.CreateChatContentItemsText(components.ChatContentText{
					Type: components.ChatContentTextTypeText,
					Text: ensureNonEmptyText(p.Text),
				}))
			case niro.KindImage:
				url := p.URL
				if url == "" && len(p.Data) > 0 {
					url = dataURI(p.Data, p.Mime)
				}
				parts = append(parts, components.CreateChatContentItemsImageURL(components.ChatContentImage{
					Type: components.ChatContentImageTypeImageURL,
					ImageURL: components.ChatContentImageImageURL{
						URL: url,
					},
				}))
			}
		}
		return components.CreateChatMessagesUser(components.ChatUserMessage{
			Role:    components.ChatUserMessageRoleUser,
			Content: components.CreateChatUserMessageContentArrayOfChatContentItems(parts),
		})

	case niro.RoleAssistant:
		text := ensureNonEmptyText(extractText(msg))
		m := components.ChatAssistantMessage{
			Role: components.ChatAssistantMessageRoleAssistant,
			Content: optionalnullable.From(ptr(components.CreateChatAssistantMessageContentStr(text))),
		}
		for _, p := range msg.Parts {
			if p.Kind == niro.KindToolCall && p.Tool != nil {
				m.ToolCalls = append(m.ToolCalls, components.ChatToolCall{
					ID:   p.Tool.ID,
					Type: components.ChatToolCallTypeFunction,
					Function: components.ChatToolCallFunction{
						Name:      p.Tool.Name,
						Arguments: string(p.Tool.Args),
					},
				})
			}
		}
		return components.CreateChatMessagesAssistant(m)

	case niro.RoleTool:
		callID := ""
		content := extractText(msg)
		if len(msg.Parts) > 0 && msg.Parts[0].Result != nil {
			callID = msg.Parts[0].Result.CallID
			content = msg.Parts[0].Result.Content
		}
		return components.CreateChatMessagesTool(components.ChatToolMessage{
			Role:       components.ChatToolMessageRoleTool,
			ToolCallID: callID,
			Content:    components.CreateChatToolMessageContentStr(ensureNonEmptyText(content)),
		})

	default:
		return components.CreateChatMessagesUser(components.ChatUserMessage{
			Role:    components.ChatUserMessageRoleUser,
			Content: components.CreateChatUserMessageContentStr(ensureNonEmptyText(extractText(msg))),
		})
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
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func rawToMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	_ = niro.JSONUnmarshal(raw, &m)
	return m
}

func splitJSONSchemaParams(raw json.RawMessage) (string, map[string]any) {
	if len(raw) == 0 {
		return "", nil
	}
	var m map[string]any
	if err := niro.JSONUnmarshal(raw, &m); err != nil {
		return "", nil
	}
	if inner, ok := m["schema"].(map[string]any); ok {
		name, _ := m["name"].(string)
		if name == "" {
			name = "response"
		}
		return name, inner
	}
	return "response", m
}

func ptr[T any](v T) *T { return &v }
