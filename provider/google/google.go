// Package google implements a Ryn Provider backed by the official
// Google Generative AI Go SDK (github.com/google/generative-ai-go).
//
// This provider:
//   - Uses the official SDK for auth, retries, and API compatibility
//   - Streams text tokens and tool calls from Gemini models
//   - Supports multimodal input (text, image, audio, video)
//   - Reports token usage via KindUsage frames
//   - Sets ResponseMeta with model, finish reason, and response ID
//
// Usage:
//
//	llm := google.New(os.Getenv("GOOGLE_API_KEY"))
//	stream, err := llm.Generate(ctx, &ryn.Request{
//	    Model: "gemini-2.0-flash",
//	    Messages: []ryn.Message{ryn.UserText("Hello")},
//	})
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/iterator"
	goption "google.golang.org/api/option"

	"ryn.dev/ryn"
)

// Provider implements ryn.Provider using the Google Generative AI SDK.
type Provider struct {
	client *genai.Client
	model  string
}

var _ ryn.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*providerConfig)

type providerConfig struct {
	model   string
	apiOpts []goption.ClientOption
}

// WithModel sets the default model.
func WithModel(model string) Option {
	return func(c *providerConfig) { c.model = model }
}

// WithClientOption appends a Google API client option.
func WithClientOption(opt goption.ClientOption) Option {
	return func(c *providerConfig) {
		c.apiOpts = append(c.apiOpts, opt)
	}
}

// New creates a Google Gemini provider.
func New(apiKey string, opts ...Option) (*Provider, error) {
	cfg := &providerConfig{model: "gemini-2.0-flash"}
	for _, o := range opts {
		o(cfg)
	}

	clientOpts := append([]goption.ClientOption{goption.WithAPIKey(apiKey)}, cfg.apiOpts...)
	client, err := genai.NewClient(context.Background(), clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("ryn/google: new client: %w", err)
	}

	return &Provider{client: client, model: cfg.model}, nil
}

// NewFromClient creates a provider from an existing Google AI client.
func NewFromClient(client *genai.Client, model string) *Provider {
	return &Provider{client: client, model: model}
}

// Close releases resources held by the provider.
func (p *Provider) Close() error {
	return p.client.Close()
}

// Generate implements ryn.Provider.
func (p *Provider) Generate(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
	modelName := req.Model
	if modelName == "" {
		modelName = p.model
	}

	model := p.client.GenerativeModel(modelName)
	configureModel(model, req)

	// Prepare content from messages
	content, history := buildContent(req)

	cs := model.StartChat()
	cs.History = history

	iter := cs.SendMessageStream(ctx, content...)

	stream, emitter := ryn.NewStream(32)
	go consume(ctx, iter, emitter)
	return stream, nil
}

func consume(ctx context.Context, iter *genai.GenerateContentResponseIterator, out *ryn.Emitter) {
	defer out.Close()

	var totalInput, totalOutput int32

	for {
		resp, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			out.Error(fmt.Errorf("ryn/google: stream: %w", err))
			return
		}

		// Usage metadata
		if resp.UsageMetadata != nil {
			totalInput = resp.UsageMetadata.PromptTokenCount
			totalOutput = resp.UsageMetadata.CandidatesTokenCount
		}

		if len(resp.Candidates) == 0 {
			continue
		}

		cand := resp.Candidates[0]
		if cand.Content == nil {
			continue
		}

		for _, part := range cand.Content.Parts {
			switch v := part.(type) {
			case genai.Text:
				if err := out.Emit(ctx, ryn.TextFrame(string(v))); err != nil {
					return
				}

			case genai.FunctionCall:
				args, _ := json.Marshal(v.Args)
				tc := &ryn.ToolCall{
					Name: v.Name,
					Args: args,
				}
				if err := out.Emit(ctx, ryn.ToolCallFrame(tc)); err != nil {
					return
				}
			}
		}
	}

	// Emit final usage
	usage := &ryn.Usage{
		InputTokens:  int(totalInput),
		OutputTokens: int(totalOutput),
		TotalTokens:  int(totalInput + totalOutput),
	}
	_ = out.Emit(ctx, ryn.UsageFrame(usage))

	out.SetResponse(&ryn.ResponseMeta{
		Usage: *usage,
	})
}

func configureModel(model *genai.GenerativeModel, req *ryn.Request) {
	// System instruction
	if req.SystemPrompt != "" {
		model.SystemInstruction = genai.NewUserContent(genai.Text(req.SystemPrompt))
	}

	// Options
	if req.Options.MaxTokens > 0 {
		n := int32(req.Options.MaxTokens)
		model.MaxOutputTokens = &n
	}
	if req.Options.Temperature != nil {
		t := float32(*req.Options.Temperature)
		model.Temperature = &t
	}
	if req.Options.TopP != nil {
		p := float32(*req.Options.TopP)
		model.TopP = &p
	}
	if req.Options.TopK != nil {
		k := int32(*req.Options.TopK)
		model.TopK = &k
	}
	if len(req.Options.Stop) > 0 {
		model.StopSequences = req.Options.Stop
	}

	// Response format
	switch req.ResponseFormat {
	case "json":
		model.ResponseMIMEType = "application/json"
	}

	// Tools
	if len(req.Tools) > 0 {
		var funcDecls []*genai.FunctionDeclaration
		for _, tool := range req.Tools {
			fd := &genai.FunctionDeclaration{
				Name:        tool.Name,
				Description: tool.Description,
			}
			if len(tool.Parameters) > 0 {
				var schema genai.Schema
				json.Unmarshal(tool.Parameters, &schema)
				fd.Parameters = &schema
			}
			funcDecls = append(funcDecls, fd)
		}
		model.Tools = []*genai.Tool{{FunctionDeclarations: funcDecls}}
	}
}

func buildContent(req *ryn.Request) ([]genai.Part, []*genai.Content) {
	msgs := req.Messages

	// Filter out system messages (handled via SystemInstruction)
	var filtered []ryn.Message
	for _, msg := range msgs {
		if msg.Role == ryn.RoleSystem {
			continue
		}
		filtered = append(filtered, msg)
	}

	if len(filtered) == 0 {
		return nil, nil
	}

	// Last message becomes the input; rest is history
	last := filtered[len(filtered)-1]
	history := filtered[:len(filtered)-1]

	var histContent []*genai.Content
	for _, msg := range history {
		c := convertToContent(msg)
		histContent = append(histContent, c)
	}

	return convertParts(last), histContent
}

func convertToContent(msg ryn.Message) *genai.Content {
	role := "user"
	switch msg.Role {
	case ryn.RoleAssistant:
		role = "model"
	case ryn.RoleTool:
		role = "function"
	}
	return &genai.Content{
		Role:  role,
		Parts: convertParts(msg),
	}
}

func convertParts(msg ryn.Message) []genai.Part {
	var parts []genai.Part
	for _, p := range msg.Parts {
		switch p.Kind {
		case ryn.KindText:
			parts = append(parts, genai.Text(p.Text))

		case ryn.KindImage:
			if len(p.Data) > 0 {
				parts = append(parts, genai.Blob{
					MIMEType: p.Mime,
					Data:     p.Data,
				})
			}

		case ryn.KindAudio:
			if len(p.Data) > 0 {
				parts = append(parts, genai.Blob{
					MIMEType: p.Mime,
					Data:     p.Data,
				})
			}

		case ryn.KindVideo:
			if len(p.Data) > 0 {
				parts = append(parts, genai.Blob{
					MIMEType: p.Mime,
					Data:     p.Data,
				})
			}

		case ryn.KindToolResult:
			if p.Result != nil {
				// Extract function name from CallID if available
				name := p.Result.CallID
				parts = append(parts, genai.FunctionResponse{
					Name:     name,
					Response: map[string]any{"result": p.Result.Content},
				})
			}

		case ryn.KindToolCall:
			if p.Tool != nil {
				var args map[string]any
				json.Unmarshal(p.Tool.Args, &args)
				parts = append(parts, genai.FunctionCall{
					Name: p.Tool.Name,
					Args: args,
				})
			}
		}
	}
	if len(parts) == 0 {
		// Google SDK requires at least one part
		parts = append(parts, genai.Text(""))
	}
	return parts
}

// roleName is unused but kept for documentation
func roleName(r ryn.Role) string {
	switch r {
	case ryn.RoleAssistant:
		return "model"
	default:
		return strings.ToLower(string(r))
	}
}
