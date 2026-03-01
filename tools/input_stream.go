package tools

import (
	"context"

	"github.com/alexedtionweb/niro-stream"
)

// InputStreamingProvider is an optional provider extension for native
// input streaming (duplex-style) where user input arrives incrementally.
//
// Providers that support realtime/duplex APIs may implement this directly.
// Core helpers will automatically prefer this path when available.
type InputStreamingProvider interface {
	GenerateInputStream(ctx context.Context, req *ryn.Request, input *ryn.Stream) (*ryn.Stream, error)
}

// InputStreamOptions controls behavior when falling back to non-native providers.
type InputStreamOptions struct {
	// Role used when converting streamed input to a single message.
	// Defaults to ryn.RoleUser.
	Role ryn.Role
}

// DefaultInputStreamOptions returns safe defaults.
func DefaultInputStreamOptions() InputStreamOptions {
	return InputStreamOptions{Role: ryn.RoleUser}
}

// GenerateInputStream calls provider-native input streaming when supported.
// Otherwise it degrades gracefully by collecting text frames from input and
// sending them as one message in a standard Generate request.
func GenerateInputStream(ctx context.Context, p ryn.Provider, req *ryn.Request, input *ryn.Stream, opts InputStreamOptions) (*ryn.Stream, error) {
	if p == nil {
		return nil, ryn.NewError(ryn.ErrCodeInvalidRequest, "provider is nil")
	}
	if req == nil {
		return nil, ryn.NewError(ryn.ErrCodeInvalidRequest, "request is nil")
	}
	if input == nil {
		return nil, ryn.NewError(ryn.ErrCodeInvalidRequest, "input stream is nil")
	}

	if sp, ok := p.(InputStreamingProvider); ok {
		return sp.GenerateInputStream(ctx, req, input)
	}

	role := opts.Role
	if role == "" {
		role = ryn.RoleUser
	}
	text, err := ryn.CollectText(ctx, input)
	if err != nil {
		return nil, err
	}

	next := *req
	next.Messages = append(append([]ryn.Message(nil), req.Messages...), ryn.Multi(role, ryn.TextPart(text)))
	return p.Generate(ctx, &next)
}
