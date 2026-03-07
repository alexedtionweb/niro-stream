// Package bedrock implements a Niro Provider backed by the AWS SDK v2
// for Amazon Bedrock Runtime (ConverseStream API).
//
// This is an opt-in provider module. Import it only when you need
// AWS Bedrock models:
//
//	go get github.com/alexedtionweb/niro-stream/provider/bedrock
//
// # SDK Access
//
// Use [Provider.Client] to access the underlying Bedrock SDK client.
// Use [WithRequestHook] or pass a [RequestHook] as Request.Extra to
// modify the raw ConverseStreamInput before each request.
//
// # Usage
//
//	cfg, _ := config.LoadDefaultConfig(ctx)
//	llm := bedrock.New(cfg)
//	stream, err := llm.Generate(ctx, &niro.Request{
//	    Model: "anthropic.claude-sonnet-4-5-20250514-v1:0",
//	    Messages: []niro.Message{niro.UserText("Hello")},
//	})
package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	smithy "github.com/aws/smithy-go"

	"github.com/alexedtionweb/niro-stream"
)

// RequestHook allows modifying the raw ConverseStreamInput before the
// request is sent. Use this to set SDK-specific parameters not exposed
// by niro.Request (guardrails, additional model fields, etc.).
//
// Provider-level:
//
//	bedrock.New(cfg, bedrock.WithRequestHook(func(in *bedrockruntime.ConverseStreamInput) {
//	    in.GuardrailConfig = &types.GuardrailConfiguration{...}
//	}))
//
// Per-request (via Request.Extra):
//
//	stream, _ := llm.Generate(ctx, &niro.Request{
//	    Messages: msgs,
//	    Extra: bedrock.RequestHook(func(in *bedrockruntime.ConverseStreamInput) { ... }),
//	})
type RequestHook func(input *bedrockruntime.ConverseStreamInput)

// Extras enables combining Bedrock-specific request customizations in Request.Extra.
//
// This is useful when you need both a RequestHook and an inference profile
// override on the same request.
type Extras struct {
	// InferenceProfile sets ConverseStreamInput.ModelId for this request.
	// Accepts inference profile ID or ARN.
	InferenceProfile string

	// Hook is an optional per-request Bedrock RequestHook.
	Hook RequestHook
}

// Provider implements niro.Provider using AWS Bedrock ConverseStream.
type Provider struct {
	client *bedrockruntime.Client
	model  string
	hooks  []RequestHook
	// inferenceProfile, when set, is used as default target in ModelId
	// when Request.Model is empty.
	inferenceProfile string
}

var _ niro.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*providerConfig)

type providerConfig struct {
	model            string
	hooks            []RequestHook
	inferenceProfile string
}

// WithModel sets the default model ID.
func WithModel(model string) Option {
	return func(c *providerConfig) { c.model = model }
}

// WithRequestHook registers a function called with the raw SDK input
// before each request. Multiple hooks are called in registration order.
func WithRequestHook(fn RequestHook) Option {
	return func(c *providerConfig) {
		c.hooks = append(c.hooks, fn)
	}
}

// WithInferenceProfile sets a default Bedrock inference profile ID/ARN.
//
// If Request.Model is empty, this value is sent as ConverseStreamInput.ModelId.
func WithInferenceProfile(profile string) Option {
	return func(c *providerConfig) {
		c.inferenceProfile = profile
	}
}

// New creates a Bedrock provider from an AWS config.
func New(cfg aws.Config, opts ...Option) *Provider {
	pc := &providerConfig{model: "anthropic.claude-sonnet-4-5-20250514-v1:0"}
	for _, o := range opts {
		o(pc)
	}
	return &Provider{
		client:           bedrockruntime.NewFromConfig(cfg),
		model:            pc.model,
		hooks:            pc.hooks,
		inferenceProfile: pc.inferenceProfile,
	}
}

// NewFromClient creates a provider from an existing Bedrock client.
func NewFromClient(client *bedrockruntime.Client, model string, opts ...Option) *Provider {
	pc := &providerConfig{model: model}
	for _, o := range opts {
		o(pc)
	}
	if pc.model == "" {
		pc.model = model
	}
	return &Provider{
		client:           client,
		model:            pc.model,
		hooks:            pc.hooks,
		inferenceProfile: pc.inferenceProfile,
	}
}

// Client returns the underlying Bedrock SDK client.
// Use for direct SDK access when the niro abstraction is insufficient.
func (p *Provider) Client() *bedrockruntime.Client { return p.client }

// CacheCaps reports Bedrock cache support characteristics.
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
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "niro/bedrock: nil request").WithProvider("bedrock")
	}
	if len(req.Messages) == 0 {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "messages required").WithProvider("bedrock")
	}
	if req.Options.ExperimentalReasoning {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "niro/bedrock: experimental reasoning is not supported").WithProvider("bedrock")
	}

	model := req.Model
	if model == "" && p.inferenceProfile != "" {
		model = p.inferenceProfile
	}
	if model == "" {
		model = p.model
	}

	cacheHint, cacheEnabled := niro.GetCacheHint(ctx)
	cacheAttempted := cacheEnabled && cacheHint.Mode != niro.CacheBypass
	cacheRequire := cacheHint.Mode == niro.CacheRequire

	var extraHook RequestHook
	if extra, ok := req.Extra.(Extras); ok {
		if extra.InferenceProfile != "" {
			model = extra.InferenceProfile
		}
		extraHook = extra.Hook
	}
	if extra, ok := req.Extra.(*Extras); ok && extra != nil {
		if extra.InferenceProfile != "" {
			model = extra.InferenceProfile
		}
		extraHook = extra.Hook
	}

	input := p.buildInput(model, req, cacheHint, cacheAttempted)

	// Provider-level hooks
	for _, h := range p.hooks {
		h(input)
	}
	// Per-request hook via Extra
	if hook, ok := req.Extra.(RequestHook); ok {
		hook(input)
	}
	if extraHook != nil {
		extraHook(input)
	}

	resp, err := p.client.ConverseStream(ctx, input)
	if err != nil {
		return nil, classifyError(err)
	}

	stream, emitter := niro.NewStream(niro.DefaultStreamBuffer)
	go consume(ctx, resp, emitter, model, cacheAttempted, cacheRequire)
	return stream, nil
}

func consume(
	ctx context.Context,
	resp *bedrockruntime.ConverseStreamOutput,
	out *niro.Emitter,
	model string,
	cacheAttempted bool,
	cacheRequire bool,
) {
	defer out.Close()

	events := resp.GetStream()
	defer events.Close()

	var (
		toolUseID   string
		toolName    string
		toolArgsBuf []byte
	)

	// meta accumulates fields from multiple events before the final SetResponse.
	meta := &niro.ResponseMeta{Model: model}

	for event := range events.Events() {
		switch ev := event.(type) {
		case *types.ConverseStreamOutputMemberContentBlockDelta:
			switch delta := ev.Value.Delta.(type) {
			case *types.ContentBlockDeltaMemberText:
				if err := out.Emit(ctx, niro.TextFrame(delta.Value)); err != nil {
					return
				}
			case *types.ContentBlockDeltaMemberToolUse:
				if delta.Value.Input != nil {
					toolArgsBuf = append(toolArgsBuf, []byte(*delta.Value.Input)...)
				}
			}

		case *types.ConverseStreamOutputMemberContentBlockStart:
			if ev.Value.Start != nil {
				if tu, ok := ev.Value.Start.(*types.ContentBlockStartMemberToolUse); ok {
					toolUseID = aws.ToString(tu.Value.ToolUseId)
					toolName = aws.ToString(tu.Value.Name)
					toolArgsBuf = toolArgsBuf[:0]
				}
			}

		case *types.ConverseStreamOutputMemberContentBlockStop:
			if toolName != "" {
				// Copy args: toolArgsBuf's backing array is reused across tool
				// calls (toolArgsBuf[:0]), so we must not alias it in RawMessage.
				argsCopy := make([]byte, len(toolArgsBuf))
				copy(argsCopy, toolArgsBuf)
				tc := &niro.ToolCall{
					ID:   toolUseID,
					Name: toolName,
					Args: json.RawMessage(argsCopy),
				}
				if err := out.Emit(ctx, niro.ToolCallFrame(tc)); err != nil {
					return
				}
				toolName = ""
				toolUseID = ""
			}

		case *types.ConverseStreamOutputMemberMetadata:
			if ev.Value.Usage != nil {
				u := ev.Value.Usage
				cacheRead := int(aws.ToInt32(u.CacheReadInputTokens))
				cacheWriteTokens := int(aws.ToInt32(u.CacheWriteInputTokens))
				usage := &niro.Usage{
					InputTokens:  int(aws.ToInt32(u.InputTokens)),
					OutputTokens: int(aws.ToInt32(u.OutputTokens)),
					TotalTokens:  int(aws.ToInt32(u.TotalTokens)),
				}
				niro.SetCacheUsageDetail(usage, cacheAttempted, cacheRead > 0, cacheWriteTokens > 0, cacheRead, 0)
				if usage.Detail != nil {
					usage.Detail["bedrock_cache_write_input_tokens"] = cacheWriteTokens
				}
				if cacheRequire && cacheAttempted && cacheRead == 0 && cacheWriteTokens == 0 {
					out.Error(niro.NewError(niro.ErrCodeProviderError, "niro/bedrock: cache required but provider reported no cache usage").WithProvider("bedrock"))
					return
				}
				_ = out.Emit(ctx, niro.UsageFrame(usage))
				meta.Usage = *usage
				if ev.Value.Metrics != nil && ev.Value.Metrics.LatencyMs != nil {
					meta.ProviderMeta = map[string]any{
						"latency_ms": *ev.Value.Metrics.LatencyMs,
					}
				}
			}

		case *types.ConverseStreamOutputMemberMessageStop:
			meta.FinishReason = mapFinishReason(ev.Value.StopReason)
		}
	}

	if err := events.Err(); err != nil {
		out.Error(classifyError(err))
		return
	}

	if meta.FinishReason == "" {
		meta.FinishReason = "stop"
	}
	out.SetResponse(meta)
}

// classifyError maps AWS SDK errors to typed *niro.Error values.
func classifyError(err error) *niro.Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return niro.WrapError(niro.ErrCodeTimeout, "request timed out", err).WithProvider("bedrock")
	}
	if errors.Is(err, context.Canceled) {
		return niro.WrapError(niro.ErrCodeContextCancelled, "context cancelled", err).WithProvider("bedrock")
	}

	var throttle *types.ThrottlingException
	if errors.As(err, &throttle) {
		return niro.WrapError(niro.ErrCodeRateLimited, throttle.ErrorMessage(), err).
			WithProvider("bedrock")
	}
	var quota *types.ServiceQuotaExceededException
	if errors.As(err, &quota) {
		return niro.WrapError(niro.ErrCodeRateLimited, quota.ErrorMessage(), err).
			WithProvider("bedrock")
	}
	var access *types.AccessDeniedException
	if errors.As(err, &access) {
		return niro.WrapError(niro.ErrCodeAuthenticationFailed, access.ErrorMessage(), err).WithProvider("bedrock")
	}
	var notFound *types.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return niro.WrapError(niro.ErrCodeModelNotFound, notFound.ErrorMessage(), err).WithProvider("bedrock")
	}
	var validation *types.ValidationException
	if errors.As(err, &validation) {
		return niro.WrapError(niro.ErrCodeInvalidRequest, validation.ErrorMessage(), err).WithProvider("bedrock")
	}
	var modelTimeout *types.ModelTimeoutException
	if errors.As(err, &modelTimeout) {
		return niro.WrapError(niro.ErrCodeTimeout, modelTimeout.ErrorMessage(), err).
			WithProvider("bedrock")
	}
	var svcUnavail *types.ServiceUnavailableException
	if errors.As(err, &svcUnavail) {
		return niro.WrapError(niro.ErrCodeServiceUnavailable, svcUnavail.ErrorMessage(), err).
			WithProvider("bedrock")
	}
	var internal *types.InternalServerException
	if errors.As(err, &internal) {
		return niro.WrapError(niro.ErrCodeProviderError, internal.ErrorMessage(), err).
			WithProvider("bedrock")
	}
	var streamErr *types.ModelStreamErrorException
	if errors.As(err, &streamErr) {
		return niro.WrapError(niro.ErrCodeStreamError, streamErr.ErrorMessage(), err).WithProvider("bedrock")
	}

	// Fallback: some error types (e.g. ServiceQuotaExceededException) are absent
	// from the ConverseStream deserializer's switch and come back as a generic
	// smithy error. Map the code string to the canonical niro error code.
	var generic *smithy.GenericAPIError
	if errors.As(err, &generic) {
		switch generic.Code {
		case "ServiceQuotaExceededException":
			return niro.WrapError(niro.ErrCodeRateLimited, generic.Message, err).WithProvider("bedrock")
		case "AccessDeniedException":
			return niro.WrapError(niro.ErrCodeAuthenticationFailed, generic.Message, err).WithProvider("bedrock")
		case "ResourceNotFoundException":
			return niro.WrapError(niro.ErrCodeModelNotFound, generic.Message, err).WithProvider("bedrock")
		case "ValidationException":
			return niro.WrapError(niro.ErrCodeInvalidRequest, generic.Message, err).WithProvider("bedrock")
		case "ModelTimeoutException":
			return niro.WrapError(niro.ErrCodeTimeout, generic.Message, err).WithProvider("bedrock")
		case "ModelStreamErrorException":
			return niro.WrapError(niro.ErrCodeStreamError, generic.Message, err).WithProvider("bedrock")
		case "ServiceUnavailableException":
			return niro.WrapError(niro.ErrCodeServiceUnavailable, generic.Message, err).WithProvider("bedrock")
		case "InternalServerException":
			return niro.WrapError(niro.ErrCodeProviderError, generic.Message, err).WithProvider("bedrock")
		}
	}

	return niro.WrapError(niro.ErrCodeProviderError, "bedrock error", err).WithProvider("bedrock")
}

// mapFinishReason converts a Bedrock StopReason to the niro canonical string.
func mapFinishReason(r types.StopReason) string {
	switch r {
	case types.StopReasonEndTurn, types.StopReasonStopSequence:
		return "stop"
	case types.StopReasonToolUse:
		return "tool_use"
	case types.StopReasonMaxTokens, types.StopReasonModelContextWindowExceeded:
		return "length"
	case types.StopReasonContentFiltered, types.StopReasonGuardrailIntervened:
		return "content_filter"
	default:
		return "other"
	}
}

func (p *Provider) buildInput(
	model string,
	req *niro.Request,
	cacheHint niro.CacheHint,
	cacheAttempted bool,
) *bedrockruntime.ConverseStreamInput {
	input := &bedrockruntime.ConverseStreamInput{
		ModelId: aws.String(model),
	}

	// System prompt
	var systemBlocks []types.SystemContentBlock
	if req.SystemPrompt != "" {
		systemBlocks = append(systemBlocks, &types.SystemContentBlockMemberText{
			Value: req.SystemPrompt,
		})
	}

	// Messages (including system messages from the list)
	for _, msg := range req.Messages {
		if msg.Role == niro.RoleSystem {
			for _, p := range msg.Parts {
				if p.Kind == niro.KindText {
					systemBlocks = append(systemBlocks, &types.SystemContentBlockMemberText{
						Value: ensureNonEmptyText(p.Text),
					})
				}
			}
			continue
		}
		input.Messages = append(input.Messages, convertMessage(msg))
	}

	if len(systemBlocks) > 0 {
		input.System = systemBlocks
	}
	if cacheAttempted {
		cachePoint := types.CachePointBlock{
			Type: types.CachePointTypeDefault,
		}
		if ttl := mapBedrockTTL(cacheHint.TTL); ttl != "" {
			cachePoint.Ttl = ttl
		}
		if len(input.System) > 0 {
			input.System = append(input.System, &types.SystemContentBlockMemberCachePoint{Value: cachePoint})
		} else if len(input.Messages) > 0 {
			msg := input.Messages[0]
			msg.Content = append(msg.Content, &types.ContentBlockMemberCachePoint{Value: cachePoint})
			input.Messages[0] = msg
		}
	}

	// Inference config
	inferenceConfig := &types.InferenceConfiguration{}
	hasConfig := false
	if req.Options.MaxTokens > 0 {
		n := int32(req.Options.MaxTokens)
		inferenceConfig.MaxTokens = &n
		hasConfig = true
	}
	if req.Options.Temperature != nil {
		t := float32(*req.Options.Temperature)
		inferenceConfig.Temperature = &t
		hasConfig = true
	}
	if req.Options.TopP != nil {
		p := float32(*req.Options.TopP)
		inferenceConfig.TopP = &p
		hasConfig = true
	}
	if len(req.Options.Stop) > 0 {
		inferenceConfig.StopSequences = req.Options.Stop
		hasConfig = true
	}
	if hasConfig {
		input.InferenceConfig = inferenceConfig
	}

	// Tools
	if len(req.Tools) > 0 {
		var toolSpecs []types.Tool
		for _, tool := range req.Tools {
			spec := &types.ToolMemberToolSpec{
				Value: types.ToolSpecification{
					Name:        aws.String(tool.Name),
					Description: aws.String(tool.Description),
				},
			}
			// InputSchema is required by the Bedrock SDK; use a minimal empty
			// object schema when the caller has not specified parameters.
			params := tool.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			var doc any
			_ = niro.JSONUnmarshal(params, &doc)
			spec.Value.InputSchema = &types.ToolInputSchemaMemberJson{
				Value: document.NewLazyDocument(doc),
			}
			toolSpecs = append(toolSpecs, spec)
		}
		toolCfg := &types.ToolConfiguration{Tools: toolSpecs}
		switch req.ToolChoice {
		case niro.ToolChoiceRequired:
			toolCfg.ToolChoice = &types.ToolChoiceMemberAny{Value: types.AnyToolChoice{}}
		case niro.ToolChoiceNone:
			// Bedrock has no "none" mode; omit ToolChoice to let the model decide.
		default:
			if name := strings.TrimPrefix(string(req.ToolChoice), niro.ToolChoiceFuncPrefix); name != "" && name != string(req.ToolChoice) {
				toolCfg.ToolChoice = &types.ToolChoiceMemberTool{
					Value: types.SpecificToolChoice{Name: aws.String(name)},
				}
			} else {
				toolCfg.ToolChoice = &types.ToolChoiceMemberAuto{Value: types.AutoToolChoice{}}
			}
		}
		input.ToolConfig = toolCfg
	}

	return input
}

func convertMessage(msg niro.Message) types.Message {
	role := types.ConversationRoleUser
	if msg.Role == niro.RoleAssistant {
		role = types.ConversationRoleAssistant
	}

	var blocks []types.ContentBlock

	for _, p := range msg.Parts {
		switch p.Kind {
		case niro.KindText:
			blocks = append(blocks, &types.ContentBlockMemberText{
				Value: ensureNonEmptyText(p.Text),
			})

		case niro.KindImage:
			if len(p.Data) > 0 {
				format := imageFormat(p.Mime)
				blocks = append(blocks, &types.ContentBlockMemberImage{
					Value: types.ImageBlock{
						Format: format,
						Source: &types.ImageSourceMemberBytes{
							Value: p.Data,
						},
					},
				})
			}

		case niro.KindToolCall:
			if p.Tool != nil {
				var input any
				_ = niro.JSONUnmarshal(p.Tool.Args, &input)
				blocks = append(blocks, &types.ContentBlockMemberToolUse{
					Value: types.ToolUseBlock{
						ToolUseId: aws.String(p.Tool.ID),
						Name:      aws.String(p.Tool.Name),
						Input:     document.NewLazyDocument(input),
					},
				})
			}

		case niro.KindToolResult:
			if p.Result != nil {
				status := types.ToolResultStatusSuccess
				if p.Result.IsError {
					status = types.ToolResultStatusError
				}
				blocks = append(blocks, &types.ContentBlockMemberToolResult{
					Value: types.ToolResultBlock{
						ToolUseId: aws.String(p.Result.CallID),
						Status:    status,
						Content: []types.ToolResultContentBlock{
							&types.ToolResultContentBlockMemberText{
								Value: ensureNonEmptyText(p.Result.Content),
							},
						},
					},
				})
			}
		}
	}

	return types.Message{Role: role, Content: blocks}
}

// ensureNonEmptyText returns s, or " " if s is empty, for API compatibility (e.g. handoff with no classifier text).
func ensureNonEmptyText(s string) string {
	if s == "" {
		return " "
	}
	return s
}

func imageFormat(mime string) types.ImageFormat {
	switch mime {
	case "image/jpeg":
		return types.ImageFormatJpeg
	case "image/gif":
		return types.ImageFormatGif
	case "image/webp":
		return types.ImageFormatWebp
	default:
		return types.ImageFormatPng
	}
}

func mapBedrockTTL(ttl time.Duration) types.CacheTTL {
	if ttl >= time.Hour {
		return types.CacheTTLOneHour
	}
	if ttl >= 5*time.Minute {
		return types.CacheTTLFiveMinutes
	}
	return ""
}
