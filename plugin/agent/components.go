package agent

import (
	"context"
	"fmt"

	"ryn.dev/ryn/component"
	"ryn.dev/ryn/registry"
	"ryn.dev/ryn/tools"
)

// ToolingComponent enables Toolset-based tool execution in agent runtime.
type ToolingComponent struct {
	Toolset *tools.Toolset
	Options tools.ToolStreamOptions
}

func (c *ToolingComponent) Name() string { return "agent.tooling" }

func (c *ToolingComponent) Capabilities() []component.Capability {
	return []component.Capability{component.CapabilityAgentTool}
}

func (c *ToolingComponent) Start(ctx context.Context) error {
	_ = ctx
	return nil
}

func (c *ToolingComponent) Close() error { return nil }

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

// MultiTenantComponent enables runtime client selection via core router.
type MultiTenantComponent struct {
	Router *registry.MultiTenantProvider
}

func (c *MultiTenantComponent) Name() string { return "agent.multitenancy" }

func (c *MultiTenantComponent) Capabilities() []component.Capability {
	return nil
}

func (c *MultiTenantComponent) Start(ctx context.Context) error {
	_ = ctx
	return nil
}

func (c *MultiTenantComponent) Close() error { return nil }

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
