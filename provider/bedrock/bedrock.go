// Package bedrock implements a Ryn Provider backed by the AWS SDK v2
// for Amazon Bedrock Runtime (ConverseStream API).
//
// This is an opt-in provider module. Import it only when you need
// AWS Bedrock models:
//
//	go get ryn.dev/ryn/provider/bedrock
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
//	stream, err := llm.Generate(ctx, &ryn.Request{
//	    Model: "anthropic.claude-sonnet-4-5-20250514-v1:0",
//	    Messages: []ryn.Message{ryn.UserText("Hello")},
//	})
package bedrock

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"ryn.dev/ryn"
)

// RequestHook allows modifying the raw ConverseStreamInput before the
// request is sent. Use this to set SDK-specific parameters not exposed
// by ryn.Request (guardrails, additional model fields, etc.).
//
// Provider-level:
//
//	bedrock.New(cfg, bedrock.WithRequestHook(func(in *bedrockruntime.ConverseStreamInput) {
//	    in.GuardrailConfig = &types.GuardrailConfiguration{...}
//	}))
//
// Per-request (via Request.Extra):
//
//	stream, _ := llm.Generate(ctx, &ryn.Request{
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

// Provider implements ryn.Provider using AWS Bedrock ConverseStream.
type Provider struct {
	client *bedrockruntime.Client
	model  string
	hooks  []RequestHook
	// inferenceProfile, when set, is used as default target in ModelId
	// when Request.Model is empty.
	inferenceProfile string
}

var _ ryn.Provider = (*Provider)(nil)

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
func NewFromClient(client *bedrockruntime.Client, model string) *Provider {
	return &Provider{client: client, model: model}
}

// Client returns the underlying Bedrock SDK client.
// Use for direct SDK access when the ryn abstraction is insufficient.
func (p *Provider) Client() *bedrockruntime.Client { return p.client }

// Generate implements ryn.Provider.
func (p *Provider) Generate(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
	model := req.Model
	if model == "" && p.inferenceProfile != "" {
		model = p.inferenceProfile
	}
	if model == "" {
		model = p.model
	}

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

	input := p.buildInput(model, req)

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
		return nil, fmt.Errorf("ryn/bedrock: converse: %w", err)
	}

	stream, emitter := ryn.NewStream(32)
	go consume(ctx, resp, emitter, model)
	return stream, nil
}

func consume(ctx context.Context, resp *bedrockruntime.ConverseStreamOutput, out *ryn.Emitter, model string) {
	defer out.Close()

	events := resp.GetStream()
	defer events.Close()

	var (
		toolUseID   string
		toolName    string
		toolArgsBuf []byte
	)

	for event := range events.Events() {
		switch ev := event.(type) {
		case *types.ConverseStreamOutputMemberContentBlockDelta:
			switch delta := ev.Value.Delta.(type) {
			case *types.ContentBlockDeltaMemberText:
				if err := out.Emit(ctx, ryn.TextFrame(delta.Value)); err != nil {
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
				tc := &ryn.ToolCall{
					ID:   toolUseID,
					Name: toolName,
					Args: json.RawMessage(toolArgsBuf),
				}
				if err := out.Emit(ctx, ryn.ToolCallFrame(tc)); err != nil {
					return
				}
				toolName = ""
				toolUseID = ""
			}

		case *types.ConverseStreamOutputMemberMetadata:
			if ev.Value.Usage != nil {
				u := ev.Value.Usage
				usage := &ryn.Usage{
					InputTokens:  int(aws.ToInt32(u.InputTokens)),
					OutputTokens: int(aws.ToInt32(u.OutputTokens)),
					TotalTokens:  int(aws.ToInt32(u.TotalTokens)),
				}
				_ = out.Emit(ctx, ryn.UsageFrame(usage))

				meta := &ryn.ResponseMeta{
					Model: model,
					Usage: *usage,
				}
				if ev.Value.Metrics != nil && ev.Value.Metrics.LatencyMs != nil {
					meta.ProviderMeta = map[string]any{
						"latency_ms": *ev.Value.Metrics.LatencyMs,
					}
				}
				out.SetResponse(meta)
			}

		case *types.ConverseStreamOutputMemberMessageStop:
			reason := string(ev.Value.StopReason)
			out.SetResponse(&ryn.ResponseMeta{
				Model:        model,
				FinishReason: reason,
			})
		}
	}

	if err := events.Err(); err != nil {
		out.Error(fmt.Errorf("ryn/bedrock: event: %w", err))
	}
}

func (p *Provider) buildInput(model string, req *ryn.Request) *bedrockruntime.ConverseStreamInput {
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
		if msg.Role == ryn.RoleSystem {
			for _, p := range msg.Parts {
				if p.Kind == ryn.KindText {
					systemBlocks = append(systemBlocks, &types.SystemContentBlockMemberText{
						Value: p.Text,
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
			if len(tool.Parameters) > 0 {
				var doc any
				_ = ryn.JSONUnmarshal(tool.Parameters, &doc)
				spec.Value.InputSchema = &types.ToolInputSchemaMemberJson{
					Value: document.NewLazyDocument(doc),
				}
			}
			toolSpecs = append(toolSpecs, spec)
		}
		input.ToolConfig = &types.ToolConfiguration{
			Tools: toolSpecs,
		}
	}

	return input
}

func convertMessage(msg ryn.Message) types.Message {
	role := types.ConversationRoleUser
	if msg.Role == ryn.RoleAssistant {
		role = types.ConversationRoleAssistant
	}

	var blocks []types.ContentBlock

	for _, p := range msg.Parts {
		switch p.Kind {
		case ryn.KindText:
			blocks = append(blocks, &types.ContentBlockMemberText{
				Value: p.Text,
			})

		case ryn.KindImage:
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

		case ryn.KindToolCall:
			if p.Tool != nil {
				var input any
				_ = ryn.JSONUnmarshal(p.Tool.Args, &input)
				blocks = append(blocks, &types.ContentBlockMemberToolUse{
					Value: types.ToolUseBlock{
						ToolUseId: aws.String(p.Tool.ID),
						Name:      aws.String(p.Tool.Name),
						Input:     document.NewLazyDocument(input),
					},
				})
			}

		case ryn.KindToolResult:
			if p.Result != nil {
				blocks = append(blocks, &types.ContentBlockMemberToolResult{
					Value: types.ToolResultBlock{
						ToolUseId: aws.String(p.Result.CallID),
						Content: []types.ToolResultContentBlock{
							&types.ToolResultContentBlockMemberText{
								Value: p.Result.Content,
							},
						},
					},
				})
			}
		}
	}

	return types.Message{Role: role, Content: blocks}
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
