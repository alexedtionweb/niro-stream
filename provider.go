package ryn

import (
	"context"
	"encoding/json"
)

// Provider generates streaming LLM responses.
//
// This is the primary interface for integrating LLM backends.
// Implementations must return a Stream that emits Frames as they
// arrive from the model — not after the full response.
//
// Provider implementations should:
//   - Emit KindText frames for each text token delta
//   - Emit KindToolCall frames for completed tool calls
//   - Emit KindUsage frames with token counts (consumed automatically by Stream)
//   - Set ResponseMeta via Emitter.SetResponse before closing
//   - Respect context cancellation
//
// Built-in: provider/openai, provider/anthropic, provider/google, provider/bedrock.
// Custom: implement this interface or use ProviderFunc.
type Provider interface {
	Generate(ctx context.Context, req *Request) (*Stream, error)
}

// ProviderFunc adapts a plain function to the Provider interface.
// Useful for ad-hoc providers, testing, and bring-your-own-model.
//
//	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
//	    s, e := ryn.NewStream(0)
//	    go func() {
//	        defer e.Close()
//	        e.Emit(ctx, ryn.TextFrame("hello from mock"))
//	    }()
//	    return s, nil
//	})
type ProviderFunc func(ctx context.Context, req *Request) (*Stream, error)

func (f ProviderFunc) Generate(ctx context.Context, req *Request) (*Stream, error) {
	return f(ctx, req)
}

// Request contains everything needed to call an LLM.
type Request struct {
	// Model identifier (e.g. "gpt-4o", "claude-sonnet-4-5", "gemini-2.0-flash").
	// If empty, the provider's default model is used.
	Model string

	// SystemPrompt is a convenience field for a single system message.
	// Prepended to Messages automatically. If you need multiple system
	// messages or interleaved system turns, use Messages directly.
	SystemPrompt string

	// Messages is the conversation history.
	// Multimodal: messages can contain text, images, audio, video.
	Messages []Message

	// Tools available for the LLM to call.
	Tools []Tool

	// ToolChoice controls how the model selects tools.
	// Default is ToolChoiceAuto.
	ToolChoice ToolChoice

	// ResponseFormat controls the output format.
	// Supported values depend on the provider:
	//   - "" (default): plain text
	//   - "json": JSON output
	//   - "json_schema": structured output (use with ResponseSchema)
	ResponseFormat string

	// ResponseSchema is a JSON Schema for structured output.
	// Only used when ResponseFormat is "json_schema".
	ResponseSchema json.RawMessage

	// Options controls generation parameters.
	Options Options
}

// EffectiveMessages returns the final message list including
// any SystemPrompt prepended as a system message.
func (r *Request) EffectiveMessages() []Message {
	if r.SystemPrompt == "" {
		return r.Messages
	}
	msgs := make([]Message, 0, len(r.Messages)+1)
	msgs = append(msgs, SystemText(r.SystemPrompt))
	msgs = append(msgs, r.Messages...)
	return msgs
}

// Options controls LLM generation parameters.
// Pointer fields distinguish "not set" from zero values.
type Options struct {
	MaxTokens        int      // Maximum output tokens
	Temperature      *float64 // Sampling temperature
	TopP             *float64 // Nucleus sampling
	TopK             *int     // Top-K sampling (Anthropic, Google)
	FrequencyPenalty *float64 // Frequency penalty (OpenAI)
	PresencePenalty  *float64 // Presence penalty (OpenAI)
	Stop             []string // Stop sequences
}

// --- Option helpers ---

// Temp returns a *float64 for use in Options.Temperature.
func Temp(v float64) *float64 { return &v }

// TopPVal returns a *float64 for use in Options.TopP.
func TopPVal(v float64) *float64 { return &v }

// TopKVal returns a *int for use in Options.TopK.
func TopKVal(v int) *int { return &v }
