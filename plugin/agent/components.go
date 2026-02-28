package agent

import (
	"context"
	"fmt"
	"time"

	"ryn.dev/ryn"
	"ryn.dev/ryn/component"
	"ryn.dev/ryn/middleware"
	"ryn.dev/ryn/registry"
	"ryn.dev/ryn/tools"
)

// ── ToolingComponent ─────────────────────────────────────────────────────────

// ToolingComponent enables Toolset-based tool execution in the agent runtime.
// It wraps rt.provider with a ToolingProvider so every Run/RunStream turn
// participates in the full tool-call loop automatically.
type ToolingComponent struct {
	Toolset *tools.Toolset
	Options tools.ToolStreamOptions
}

func (c *ToolingComponent) Name() string { return "agent.tooling" }

func (c *ToolingComponent) Capabilities() []component.Capability {
	return []component.Capability{component.CapabilityAgentTool}
}

func (c *ToolingComponent) Start(ctx context.Context) error { _ = ctx; return nil }
func (c *ToolingComponent) Close() error                    { return nil }

func (c *ToolingComponent) Apply(rt *Runtime) error {
	if rt == nil || rt.provider == nil {
		return fmt.Errorf("agent.tooling: runtime/provider is nil")
	}
	set := c.Toolset
	if set == nil {
		set = tools.NewToolset()
	}
	rt.provider = tools.NewToolingProvider(rt.provider, set, c.Options)
	return nil
}

// ── MultiTenantComponent ──────────────────────────────────────────────────────

// MultiTenantComponent replaces the runtime provider with a multi-tenant
// router so the correct underlying client is selected per request.
type MultiTenantComponent struct {
	Router *registry.MultiTenantProvider
}

func (c *MultiTenantComponent) Name() string                         { return "agent.multitenancy" }
func (c *MultiTenantComponent) Capabilities() []component.Capability { return nil }
func (c *MultiTenantComponent) Start(ctx context.Context) error      { _ = ctx; return nil }
func (c *MultiTenantComponent) Close() error                         { return nil }

func (c *MultiTenantComponent) Apply(rt *Runtime) error {
	if rt == nil {
		return fmt.Errorf("agent.multitenancy: runtime is nil")
	}
	if c.Router == nil {
		return fmt.Errorf("agent.multitenancy: router is nil")
	}
	rt.provider = c.Router
	return nil
}

// ── RetryComponent ────────────────────────────────────────────────────────────

// RetryComponent wraps the runtime provider with automatic retry logic.
// Use this to transparently recover from transient provider errors (rate
// limits, network blips) without changing call sites.
//
//	agent.New(p,
//	    agent.WithComponent(&agent.RetryComponent{
//	        Config: middleware.DefaultRetryConfig(),
//	    }),
//	)
type RetryComponent struct {
	// Config controls retry behaviour. Defaults to middleware.DefaultRetryConfig().
	Config middleware.RetryConfig
}

func (c *RetryComponent) Name() string                         { return "agent.retry" }
func (c *RetryComponent) Capabilities() []component.Capability { return nil }
func (c *RetryComponent) Start(ctx context.Context) error      { _ = ctx; return nil }
func (c *RetryComponent) Close() error                         { return nil }

func (c *RetryComponent) Apply(rt *Runtime) error {
	if rt == nil || rt.provider == nil {
		return fmt.Errorf("agent.retry: runtime/provider is nil")
	}
	cfg := c.Config
	if cfg.MaxAttempts <= 0 {
		cfg = middleware.DefaultRetryConfig()
	}
	rt.provider = middleware.NewRetryProvider(rt.provider, cfg)
	return nil
}

// ── CacheComponent ────────────────────────────────────────────────────────────

// CacheComponent wraps the runtime provider with a response cache.
// Identical requests (same messages, model, options) are served from the
// cache instead of making a live API call — useful for development,
// testing, and high-QPS read-heavy workloads.
//
//	agent.New(p,
//	    agent.WithComponent(&agent.CacheComponent{
//	        Options: middleware.CacheOptions{MaxEntries: 512, TTL: 5 * time.Minute},
//	    }),
//	)
type CacheComponent struct {
	// Options configures the cache. Zero-value uses library defaults.
	Options middleware.CacheOptions
}

func (c *CacheComponent) Name() string                         { return "agent.cache" }
func (c *CacheComponent) Capabilities() []component.Capability { return nil }
func (c *CacheComponent) Start(ctx context.Context) error      { _ = ctx; return nil }
func (c *CacheComponent) Close() error                         { return nil }

func (c *CacheComponent) Apply(rt *Runtime) error {
	if rt == nil || rt.provider == nil {
		return fmt.Errorf("agent.cache: runtime/provider is nil")
	}
	rt.provider = middleware.NewCache(c.Options).Wrap(rt.provider)
	return nil
}

// ── TimeoutComponent ──────────────────────────────────────────────────────────

// TimeoutComponent wraps the runtime provider with a per-request timeout.
// Prevents runaway generations from blocking indefinitely.
//
//	agent.New(p,
//	    agent.WithComponent(&agent.TimeoutComponent{
//	        Timeout: 30 * time.Second,
//	    }),
//	)
type TimeoutComponent struct {
	// Timeout is the maximum duration for a single Generate call.
	// Defaults to middleware.DefaultTimeoutConfig().GenerationTimeout (5 min).
	Timeout time.Duration
}

func (c *TimeoutComponent) Name() string                         { return "agent.timeout" }
func (c *TimeoutComponent) Capabilities() []component.Capability { return nil }
func (c *TimeoutComponent) Start(ctx context.Context) error      { _ = ctx; return nil }
func (c *TimeoutComponent) Close() error                         { return nil }

func (c *TimeoutComponent) Apply(rt *Runtime) error {
	if rt == nil || rt.provider == nil {
		return fmt.Errorf("agent.timeout: runtime/provider is nil")
	}
	d := c.Timeout
	if d <= 0 {
		d = middleware.DefaultTimeoutConfig().GenerationTimeout
	}
	rt.provider = middleware.NewTimeoutProvider(rt.provider, d)
	return nil
}

// ── MiddlewareComponent ───────────────────────────────────────────────────────

// MiddlewareComponent is a generic escape hatch: it wraps the runtime provider
// with any arbitrary function that satisfies func(ryn.Provider) ryn.Provider.
//
// Use this for one-off decorators — tracing adapters, cost-tracking wrappers,
// test doubles, etc. — without having to implement the full Component interface.
//
//	agent.New(p,
//	    agent.WithComponent(&agent.MiddlewareComponent{
//	        Name_: "my.tracer",
//	        Fn:    func(p ryn.Provider) ryn.Provider { return myTracer{p} },
//	    }),
//	)
type MiddlewareComponent struct {
	// Name_ is the component name (must be unique in the Host).
	// Defaults to "agent.middleware" if empty.
	Name_ string

	// Fn wraps the current provider and returns a new one.
	Fn func(ryn.Provider) ryn.Provider
}

func (c *MiddlewareComponent) Name() string {
	if c.Name_ != "" {
		return c.Name_
	}
	return "agent.middleware"
}

func (c *MiddlewareComponent) Capabilities() []component.Capability { return nil }
func (c *MiddlewareComponent) Start(ctx context.Context) error      { _ = ctx; return nil }
func (c *MiddlewareComponent) Close() error                         { return nil }

func (c *MiddlewareComponent) Apply(rt *Runtime) error {
	if rt == nil || rt.provider == nil {
		return fmt.Errorf("%s: runtime/provider is nil", c.Name())
	}
	if c.Fn == nil {
		return fmt.Errorf("%s: Fn is nil", c.Name())
	}
	rt.provider = c.Fn(rt.provider)
	return nil
}
