package ryn

import "context"

// Provider generates streaming LLM responses.
//
// This is the primary interface for integrating LLM backends.
// Implementations must return a Stream that emits Frames as
// they arrive from the model — not after the full response.
//
// Built-in providers: provider/openai.
// Custom providers: implement this interface or use ProviderFunc.
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
	// Model identifier (e.g. "gpt-4o", "claude-3.5-sonnet").
	// If empty, the provider's default model is used.
	Model string

	// Messages is the conversation history.
	// Multimodal: messages can contain text, images, audio, video.
	Messages []Message

	// Tools available for the LLM to call.
	Tools []Tool

	// Options controls generation parameters.
	Options Options
}

// Options controls LLM generation parameters.
// Pointer fields distinguish "not set" from zero values.
type Options struct {
	MaxTokens   int
	Temperature *float64
	TopP        *float64
	Stop        []string
}

// --- Option helpers ---

// Temp returns a *float64 for use in Options.Temperature.
func Temp(v float64) *float64 { return &v }

// TopPVal returns a *float64 for use in Options.TopP.
func TopPVal(v float64) *float64 { return &v }
