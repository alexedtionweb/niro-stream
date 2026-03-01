// Package google implements a Niro Provider backed by the unified
// Google GenAI Go SDK (google.golang.org/genai).
//
// It supports two backends:
//
//   - Google AI (Gemini API / AI Studio): authenticated with an API key.
//   - Vertex AI: authenticated with Application Default Credentials.
//
// # Google AI
//
//	llm, _ := google.New(os.Getenv("GEMINI_API_KEY"))
//	defer llm.Close()
//
// # Vertex AI
//
//	llm, _ := google.NewVertexAI("my-project", "us-central1")
//	defer llm.Close()
//
// # Gemini Live (real-time bidirectional)
//
// Gemini Live uses a WebSocket-based protocol that is separate from the
// request/response Provider interface. Access the raw SDK client to use it:
//
//	session, err := llm.Client().Live.Connect(ctx, model, nil)
//
// # SDK hooks
//
// Use [WithRequestHook] to inspect or modify the GenerateContentConfig before
// each call. Use [RequestHook] as Request.Extra for per-request overrides.
package google

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strings"

	"google.golang.org/genai"

	"github.com/alexedtionweb/niro-stream"
)

// RequestHook lets callers inspect or modify the GenerateContentConfig and
// model name before every generate call.
//
// Provider-level (all requests):
//
//	google.New(key, google.WithRequestHook(func(model string, cfg *genai.GenerateContentConfig) {
//	    cfg.CandidateCount = 1
//	}))
//
// Per-request (via Request.Extra):
//
//	stream, _ := llm.Generate(ctx, &niro.Request{
//	    Messages: msgs,
//	    Extra: google.RequestHook(func(model string, cfg *genai.GenerateContentConfig) { ... }),
//	})
type RequestHook func(model string, cfg *genai.GenerateContentConfig)

// Provider implements niro.Provider using the Google GenAI SDK.
type Provider struct {
	client *genai.Client
	model  string
	hooks  []RequestHook
}

var _ niro.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*providerConfig)

type providerConfig struct {
	model       string
	httpClient  *http.Client
	httpOptions *genai.HTTPOptions
	hooks       []RequestHook
}

// WithModel sets the default model name (default: "gemini-2.0-flash").
func WithModel(model string) Option {
	return func(c *providerConfig) { c.model = model }
}

// WithHTTPClient sets a custom *http.Client for all requests.
// Primarily useful for testing (e.g. httptest.NewServer) and proxy setups.
func WithHTTPClient(client *http.Client) Option {
	return func(c *providerConfig) { c.httpClient = client }
}

// WithHTTPOptions sets low-level HTTP options on the underlying client.
// Use HTTPOptions.BaseURL to point at a test server or a proxy endpoint.
func WithHTTPOptions(opts genai.HTTPOptions) Option {
	return func(c *providerConfig) { c.httpOptions = &opts }
}

// WithRequestHook registers a hook invoked with the model name and
// GenerateContentConfig before every request. Multiple hooks run in order.
func WithRequestHook(fn RequestHook) Option {
	return func(c *providerConfig) { c.hooks = append(c.hooks, fn) }
}

// ---------------------------------------------------------------------------
// Constructors
// ---------------------------------------------------------------------------

// New creates a Google AI (Gemini API / AI Studio) provider.
//
// apiKey must be non-empty. Get one at https://ai.google.dev/gemini-api/docs/api-key.
// The GEMINI_API_KEY or GOOGLE_API_KEY environment variable is also read
// automatically by the SDK if the key is not supplied here.
func New(apiKey string, opts ...Option) (*Provider, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, niro.NewError(niro.ErrCodeAuthenticationFailed,
			"niro/google: API key is required").WithProvider("google")
	}

	cfg := applyOpts(opts)

	cc := &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	}
	applyHTTP(cc, cfg)

	client, err := genai.NewClient(context.Background(), cc)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError,
			"niro/google: create client", err).WithProvider("google")
	}
	return &Provider{client: client, model: cfg.model, hooks: cfg.hooks}, nil
}

// NewVertexAI creates a Vertex AI provider for the given GCP project and
// region. Authentication uses Application Default Credentials (ADC).
//
// Run `gcloud auth application-default login` to set up local credentials.
// In Cloud Run / GKE the workload identity is picked up automatically.
//
// projectID: GCP project (or set GOOGLE_CLOUD_PROJECT env var).
// location:  GCP region, e.g. "us-central1" (or set GOOGLE_CLOUD_LOCATION).
func NewVertexAI(projectID, location string, opts ...Option) (*Provider, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest,
			"niro/google: Vertex AI project ID is required").WithProvider("google")
	}
	if strings.TrimSpace(location) == "" {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest,
			"niro/google: Vertex AI location is required").WithProvider("google")
	}

	cfg := applyOpts(opts)

	cc := &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  projectID,
		Location: location,
	}
	applyHTTP(cc, cfg)

	client, err := genai.NewClient(context.Background(), cc)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError,
			"niro/google: create Vertex AI client", err).WithProvider("google")
	}
	return &Provider{client: client, model: cfg.model, hooks: cfg.hooks}, nil
}

// NewFromClient creates a provider from a pre-built *genai.Client.
// Use this when you need full control over client construction (custom
// credentials, interceptors, etc.).
func NewFromClient(client *genai.Client, model string, hooks ...RequestHook) *Provider {
	return &Provider{client: client, model: model, hooks: hooks}
}

// ---------------------------------------------------------------------------
// Provider interface
// ---------------------------------------------------------------------------

// Client returns the underlying *genai.Client.
//
// Use this to access APIs not exposed by the niro.Provider interface, such as
// Gemini Live real-time sessions:
//
//	session, err := provider.Client().Live.Connect(ctx, model, config)
func (p *Provider) Client() *genai.Client { return p.client }

// Close is a no-op for this provider. The underlying HTTP client used by
// the Google GenAI SDK is stateless and requires no explicit teardown.
// The method exists to satisfy common io.Closer patterns.
func (p *Provider) Close() error { return nil }

// Generate implements niro.Provider.
func (p *Provider) Generate(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
	if req == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "niro/google: nil request").WithProvider("google")
	}
	if req.Options.ExperimentalReasoning {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "niro/google: experimental reasoning is not supported").WithProvider("google")
	}

	if err := req.Validate(); err != nil {
		return nil, err
	}

	modelName := req.Model
	if modelName == "" {
		modelName = p.model
	}

	config := buildConfig(req)

	for _, h := range p.hooks {
		h(modelName, config)
	}
	if hook, ok := req.Extra.(RequestHook); ok {
		hook(modelName, config)
	}

	contents := buildContents(req)
	if len(contents) == 0 {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest,
			"niro/google: request must contain at least one non-system message").
			WithProvider("google")
	}

	seq := p.client.Models.GenerateContentStream(ctx, modelName, contents, config)

	stream, emitter := niro.NewStream(32)
	go consume(ctx, seq, emitter, modelName)
	return stream, nil
}

// ---------------------------------------------------------------------------
// Internal: stream consumer
// ---------------------------------------------------------------------------

func consume(
	ctx context.Context,
	seq iter.Seq2[*genai.GenerateContentResponse, error],
	out *niro.Emitter,
	modelName string,
) {
	defer out.Close()

	var totalInput, totalOutput int32
	var finishReason string
	var modelVersion string
	var responseID string
	var callIndex int

	for resp, err := range seq {
		if err != nil {
			out.Error(classifyError(err))
			return
		}

		if resp.UsageMetadata != nil {
			totalInput = resp.UsageMetadata.PromptTokenCount
			totalOutput = resp.UsageMetadata.CandidatesTokenCount
		}
		if resp.ModelVersion != "" {
			modelVersion = resp.ModelVersion
		}
		if resp.ResponseID != "" {
			responseID = resp.ResponseID
		}

		if len(resp.Candidates) == 0 {
			continue
		}
		cand := resp.Candidates[0]

		if cand.FinishReason != "" && cand.FinishReason != genai.FinishReasonUnspecified {
			finishReason = mapFinishReason(cand.FinishReason)
		}

		if cand.Content == nil {
			continue
		}

		for _, part := range cand.Content.Parts {
			if part.Text != "" {
				if err := out.Emit(ctx, niro.TextFrame(part.Text)); err != nil {
					return
				}
			} else if part.FunctionCall != nil {
				fc := part.FunctionCall
				// Use the SDK-provided call ID when available (Gemini assigns
				// these in the new SDK). Fall back to a name-based ID.
				id := fc.ID
				if id == "" {
					id = fc.Name
					if callIndex > 0 {
						id = fmt.Sprintf("%s_%d", fc.Name, callIndex)
					}
				}
				callIndex++

				args, _ := niro.JSONMarshal(fc.Args)
				tc := &niro.ToolCall{ID: id, Name: fc.Name, Args: args}
				if err := out.Emit(ctx, niro.ToolCallFrame(tc)); err != nil {
					return
				}
			}
		}
	}

	if finishReason == "" {
		finishReason = "stop"
	}
	if modelVersion == "" {
		modelVersion = modelName
	}

	usage := &niro.Usage{
		InputTokens:  int(totalInput),
		OutputTokens: int(totalOutput),
		TotalTokens:  int(totalInput + totalOutput),
	}
	_ = out.Emit(ctx, niro.UsageFrame(usage))
	out.SetResponse(&niro.ResponseMeta{
		Model:        modelVersion,
		FinishReason: finishReason,
		ID:           responseID,
		Usage:        *usage,
	})
}

// ---------------------------------------------------------------------------
// Internal: error classification
// ---------------------------------------------------------------------------

// classifyError maps a raw SDK error to a typed *niro.Error with the correct
// error code, retryability flag, and provider tag.
func classifyError(err error) *niro.Error {
	// New SDK returns genai.APIError (value type) for HTTP errors.
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		code := niro.ConvertHTTPStatusToCode(apiErr.Code)
		msg := firstLine(apiErr.Message)
		if msg == "" {
			msg = apiErr.Status
		}
		// Wrap a compact cause instead of the raw apiErr.
		// genai.APIError.Error() dumps the full multiline Message plus a
		// Details: []map[string]any rendering, producing unreadable noise.
		// The compact wrapper keeps Error() clean while still allowing callers
		// to reach the raw SDK error via errors.As(err, &genai.APIError{}).
		return niro.WrapError(code, msg, &compactAPIErr{raw: apiErr}).
			WithProvider("google").
			WithStatusCode(apiErr.Code)
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return niro.WrapError(niro.ErrCodeTimeout, "request timed out", err).
			WithProvider("google")
	}
	if errors.Is(err, context.Canceled) {
		return niro.WrapError(niro.ErrCodeContextCancelled, "context cancelled", err).
			WithProvider("google")
	}
	return niro.WrapError(niro.ErrCodeStreamError, "stream error", err).
		WithProvider("google")
}

// compactAPIErr wraps a genai.APIError with a short Error() string so that
// niro.Error.Error() doesn't duplicate the verbose message or dump Details.
// The raw error is still reachable via errors.As.
type compactAPIErr struct{ raw genai.APIError }

func (e *compactAPIErr) Error() string {
	if e.raw.Status != "" {
		return fmt.Sprintf("HTTP %d (%s)", e.raw.Code, e.raw.Status)
	}
	return fmt.Sprintf("HTTP %d", e.raw.Code)
}

// As implements errors.As so callers can still do errors.As(err, &genai.APIError{}).
func (e *compactAPIErr) As(target any) bool {
	t, ok := target.(*genai.APIError)
	if !ok {
		return false
	}
	*t = e.raw
	return true
}

// firstLine returns the first non-empty line of s, trimmed of whitespace.
// Google API error messages are multi-line; the first line is the human summary.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// mapFinishReason converts a genai.FinishReason to the niro canonical string.
func mapFinishReason(r genai.FinishReason) string {
	switch r {
	case genai.FinishReasonStop:
		return "stop"
	case genai.FinishReasonMaxTokens:
		return "length"
	case genai.FinishReasonSafety,
		genai.FinishReasonRecitation,
		genai.FinishReasonLanguage,
		genai.FinishReasonBlocklist,
		genai.FinishReasonProhibitedContent,
		genai.FinishReasonSPII,
		genai.FinishReasonImageSafety:
		return "content_filter"
	case genai.FinishReasonMalformedFunctionCall,
		genai.FinishReasonOther:
		return "other"
	default:
		return "stop"
	}
}

// ---------------------------------------------------------------------------
// Internal: request building
// ---------------------------------------------------------------------------

// buildConfig converts a niro.Request into a *genai.GenerateContentConfig.
func buildConfig(req *niro.Request) *genai.GenerateContentConfig {
	cfg := &genai.GenerateContentConfig{}

	if req.SystemPrompt != "" {
		cfg.SystemInstruction = &genai.Content{
			Role:  "user",
			Parts: []*genai.Part{{Text: req.SystemPrompt}},
		}
	}

	if req.Options.MaxTokens > 0 {
		cfg.MaxOutputTokens = int32(req.Options.MaxTokens)
	}
	if req.Options.Temperature != nil {
		t := float32(*req.Options.Temperature)
		cfg.Temperature = &t
	}
	if req.Options.TopP != nil {
		p := float32(*req.Options.TopP)
		cfg.TopP = &p
	}
	if req.Options.TopK != nil {
		k := float32(*req.Options.TopK)
		cfg.TopK = &k
	}
	if len(req.Options.Stop) > 0 {
		cfg.StopSequences = req.Options.Stop
	}

	// Response format
	switch req.ResponseFormat {
	case "json":
		cfg.ResponseMIMEType = "application/json"
	case "json_schema":
		cfg.ResponseMIMEType = "application/json"
		if len(req.ResponseSchema) > 0 {
			var schema genai.Schema
			if err := niro.JSONUnmarshal(req.ResponseSchema, &schema); err == nil {
				cfg.ResponseSchema = &schema
			}
		}
	}

	// Tools + ToolChoice
	if len(req.Tools) > 0 {
		var decls []*genai.FunctionDeclaration
		for _, t := range req.Tools {
			fd := &genai.FunctionDeclaration{
				Name:        t.Name,
				Description: t.Description,
			}
			if len(t.Parameters) > 0 {
				var schema genai.Schema
				if err := niro.JSONUnmarshal(t.Parameters, &schema); err == nil {
					fd.Parameters = &schema
				}
			}
			decls = append(decls, fd)
		}
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: decls}}

		switch req.ToolChoice {
		case niro.ToolChoiceNone:
			cfg.ToolConfig = &genai.ToolConfig{
				FunctionCallingConfig: &genai.FunctionCallingConfig{
					Mode: genai.FunctionCallingConfigModeNone,
				},
			}
		case niro.ToolChoiceRequired:
			cfg.ToolConfig = &genai.ToolConfig{
				FunctionCallingConfig: &genai.FunctionCallingConfig{
					Mode: genai.FunctionCallingConfigModeAny,
				},
			}
		default:
			// ToolChoiceAuto — SDK default, no explicit config needed.
			// ToolChoiceFunc("name") — restrict to a single function.
			if s := string(req.ToolChoice); strings.HasPrefix(s, "func:") {
				name := strings.TrimPrefix(s, "func:")
				cfg.ToolConfig = &genai.ToolConfig{
					FunctionCallingConfig: &genai.FunctionCallingConfig{
						Mode:                 genai.FunctionCallingConfigModeAny,
						AllowedFunctionNames: []string{name},
					},
				}
			}
		}
	}

	return cfg
}

// buildContents converts Request.Messages into []*genai.Content for the API.
// System messages are handled via GenerateContentConfig.SystemInstruction
// and are excluded from the content array.
func buildContents(req *niro.Request) []*genai.Content {
	var contents []*genai.Content
	for _, msg := range req.Messages {
		if msg.Role == niro.RoleSystem {
			continue
		}
		c := convertToContent(msg)
		if c != nil {
			contents = append(contents, c)
		}
	}
	return contents
}

// convertToContent maps a niro.Message to a *genai.Content.
//
// Role mapping:
//   - RoleUser      → "user"
//   - RoleAssistant → "model"
//   - RoleTool      → "user"  (FunctionResponse must be in a "user" turn per SDK)
func convertToContent(msg niro.Message) *genai.Content {
	role := "user"
	if msg.Role == niro.RoleAssistant {
		role = "model"
	}
	parts := convertParts(msg)
	if len(parts) == 0 {
		return nil
	}
	return &genai.Content{Role: role, Parts: parts}
}

// convertParts converts message parts to []*genai.Part.
// An empty text part is added when the converted list would otherwise be empty
// to satisfy the SDK requirement of at least one part per Content.
func convertParts(msg niro.Message) []*genai.Part {
	var parts []*genai.Part
	for _, p := range msg.Parts {
		switch p.Kind {
		case niro.KindText:
			parts = append(parts, &genai.Part{Text: p.Text})

		case niro.KindImage, niro.KindAudio, niro.KindVideo:
			if len(p.Data) > 0 {
				parts = append(parts, &genai.Part{
					InlineData: &genai.Blob{MIMEType: p.Mime, Data: p.Data},
				})
			}

		case niro.KindToolResult:
			if p.Result != nil {
				// Gemini API convention: use "output" key for results and
				// "error" key for errors. The model understands both.
				var response map[string]any
				if p.Result.IsError {
					response = map[string]any{"error": p.Result.Content}
				} else {
					response = map[string]any{"output": p.Result.Content}
				}
				parts = append(parts, &genai.Part{
					FunctionResponse: &genai.FunctionResponse{
						// ID correlates this response with the FunctionCall.ID.
						// Name should be the function name; we store the call ID
						// there since it encodes the name (see consume()).
						ID:       p.Result.CallID,
						Name:     p.Result.CallID,
						Response: response,
					},
				})
			}

		case niro.KindToolCall:
			if p.Tool != nil {
				var args map[string]any
				_ = niro.JSONUnmarshal(p.Tool.Args, &args)
				parts = append(parts, &genai.Part{
					FunctionCall: &genai.FunctionCall{
						ID:   p.Tool.ID,
						Name: p.Tool.Name,
						Args: args,
					},
				})
			}
		}
	}
	if len(parts) == 0 {
		parts = append(parts, &genai.Part{Text: ""})
	}
	return parts
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func applyOpts(opts []Option) *providerConfig {
	cfg := &providerConfig{model: "gemini-2.0-flash"}
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

func applyHTTP(cc *genai.ClientConfig, cfg *providerConfig) {
	if cfg.httpClient != nil {
		cc.HTTPClient = cfg.httpClient
	}
	if cfg.httpOptions != nil {
		cc.HTTPOptions = *cfg.httpOptions
	}
}
