package dsl

import (
	"context"
	"maps"
	"sync"
)

type runContextKey struct{}

// WithRunContext attaches runCtx to ctx so tool handlers (e.g. HTTP) can expand
// URL/headers with the same template data (session, event, etc.) as prompts.
func WithRunContext(ctx context.Context, runCtx *RunContext) context.Context {
	if runCtx == nil {
		return ctx
	}
	return context.WithValue(ctx, runContextKey{}, runCtx)
}

// RunContextFrom returns the RunContext attached to ctx by WithRunContext, or nil.
func RunContextFrom(ctx context.Context) *RunContext {
	if ctx == nil {
		return nil
	}
	v := ctx.Value(runContextKey{})
	if v == nil {
		return nil
	}
	r, _ := v.(*RunContext)
	return r
}

// RunContext holds user-defined variables for template expansion and expr evaluation.
// Reserved keys: "session", "event", "history". Caller may set any additional keys.
// Both the prompt template and when/unless conditions read from RunContext.
// Safe for concurrent use if each invocation uses its own RunContext.
type RunContext struct {
	mu sync.RWMutex
	m  map[string]any
}

// NewRunContext creates an empty run context.
func NewRunContext() *RunContext {
	return &RunContext{m: make(map[string]any)}
}

// Get returns the value for key and whether it was set.
func (c *RunContext) Get(key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[key]
	return v, ok
}

// GetAs returns the value for key from runCtx as type T and whether it was set and convertible.
// Use when the expected type is known to avoid manual type assertions.
func GetAs[T any](runCtx *RunContext, key string) (T, bool) {
	if runCtx == nil {
		var zero T
		return zero, false
	}
	v, ok := runCtx.Get(key)
	if !ok {
		var zero T
		return zero, false
	}
	t, ok := v.(T)
	return t, ok
}

// Set sets a variable by key. Use reserved keys "session", "event", "history"
// for template and expr; set custom keys as needed (e.g. "user").
func (c *RunContext) Set(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]any)
	}
	c.m[key] = value
}

// Snapshot returns a copy of the context as a map (e.g. for Sequence step cloning).
func (c *RunContext) Snapshot() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]any, len(c.m))
	maps.Copy(out, c.m)
	return out
}

// reserved keys used by when/unless and templates; must never be nil in envMap so expr can safely access e.g. session.disable_cep.
var reservedEnvDefaults = map[string]any{
	"session": map[string]any{},
	"event":   map[string]any{},
	"history": "",
}

// envMap returns a snapshot of the context as a map for expr.Run.
// Reserved keys (session, event, history) are always present with safe defaults so when/unless expressions
// like "session.disable_cep == true" never see nil and can evaluate without error.
func (c *RunContext) envMap() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]any, len(reservedEnvDefaults)+len(c.m))
	maps.Copy(out, reservedEnvDefaults)
	maps.Copy(out, c.m)
	return out
}
