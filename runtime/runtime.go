// Package runtime manages the lifecycle of a Provider with optional hooks and
// a post-processing Pipeline. It is the top-level entry point for traced,
// observable LLM interactions.
package runtime

import (
	"context"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/hook"
	"github.com/alexedtionweb/niro-stream/pipe"
)

// Runtime manages the lifecycle of a Provider with hooks and
// an optional post-processing Pipeline.
//
// For simple use cases, calling Provider.Generate directly is fine.
// Runtime adds value when you want:
//   - Telemetry hooks on every generation
//   - Consistent post-processing pipeline
type Runtime struct {
	provider         niro.Provider
	pipeline         *pipe.Pipeline
	hook             hook.Hook
	cacheEngine      niro.CacheEngine
	prefixNormalizer niro.PrefixNormalizer
}

// New creates a Runtime with the given Provider.
func New(p niro.Provider) *Runtime {
	return &Runtime{provider: p}
}

// WithPipeline attaches a post-processing Pipeline to the Runtime.
func (r *Runtime) WithPipeline(p *pipe.Pipeline) *Runtime {
	r.pipeline = p
	return r
}

// WithHook attaches a telemetry/observability Hook.
// Use hook.Compose() to combine multiple hooks.
func (r *Runtime) WithHook(h hook.Hook) *Runtime {
	r.hook = h
	return r
}

// WithCacheEngine attaches an optional cache engine for provider adapters.
func (r *Runtime) WithCacheEngine(engine niro.CacheEngine) *Runtime {
	r.cacheEngine = engine
	return r
}

// WithPrefixNormalizer sets a deterministic prefix normalizer for cache key derivation.
func (r *Runtime) WithPrefixNormalizer(normalizer niro.PrefixNormalizer) *Runtime {
	r.prefixNormalizer = normalizer
	return r
}

const defaultModelLabel = "(default)" // hook metadata when req.Model is empty

// Generate sends a request to the provider and returns a stream
// of frames, optionally processed through the attached pipeline.
// Hooks are invoked at each stage.
func (r *Runtime) Generate(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
	if req == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "request is nil")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}

	start := time.Now()

	model := req.Model
	if model == "" {
		model = defaultModelLabel
	}

	// Cache fast path: a single predictable nil-check.
	if req.Options.Cache != nil {
		hint, cerr := niro.NormalizeCacheOptions(req, r.prefixNormalizer)
		if cerr != nil {
			return nil, cerr
		}
		caps := niro.ProviderCacheCaps(r.provider)
		if hint.Mode == niro.CacheRequire && !caps.SupportsHint(hint) {
			return nil, niro.NewError(niro.ErrCodeInvalidRequest, "cache required but provider cannot satisfy requested cache semantics")
		}
		if hint.Mode != niro.CacheBypass && caps.SupportsPrefix {
			// Key derivation is done once per Generate call and propagated through ctx.
			ctx = niro.AttachCacheContext(ctx, hint, r.cacheEngine)
		}
	}

	// Hook: start
	if r.hook != nil {
		ctx = r.hook.OnGenerateStart(ctx, hook.GenerateStartInfo{
			Model:    model,
			Messages: len(req.Messages),
			Tools:    len(req.Tools),
		})
	}

	stream, err := r.provider.Generate(ctx, req)
	if err != nil {
		if r.hook != nil {
			r.hook.OnError(ctx, err)
			r.hook.OnGenerateEnd(ctx, hook.GenerateEndInfo{
				Model:    model,
				Duration: time.Since(start),
				Error:    err,
			})
		}
		return nil, err
	}

	if r.pipeline != nil {
		stream = r.pipeline.Run(ctx, stream)
	}

	// If we have a hook, wrap the stream to intercept frames and
	// fire OnGenerateEnd when the stream is exhausted.
	if r.hook != nil {
		stream = hook.WrapStream(ctx, stream, r.hook, model, start)
	}

	return stream, nil
}

