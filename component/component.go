// Package component provides the Component interface and ComponentHost
// for managing pluggable runtime extensions with lifecycle management.
package component

import (
	"context"
	"fmt"
	"sync"
)

// Capability identifies a component capability.
type Capability string

const (
	CapabilityAgentMemory Capability = "agent.memory"
	CapabilityAgentPeer   Capability = "agent.peer"
	CapabilityAgentMCP    Capability = "agent.mcp"
	CapabilityAgentTool   Capability = "agent.tooling"
)

// Component is a pluggable runtime extension.
//
// Components live outside core business logic, but can be mounted into a
// Host and started/stopped with app lifecycle.
type Component interface {
	Name() string
	Capabilities() []Capability
	Start(ctx context.Context) error
	Close() error
}

// Host stores and manages lifecycle for registered components.
// It is goroutine-safe.
type Host struct {
	mu    sync.RWMutex
	items map[string]Component
}

// NewHost creates an empty component host.
func NewHost() *Host {
	return &Host{items: make(map[string]Component)}
}

// Register adds a component. Names must be unique.
func (h *Host) Register(c Component) error {
	if h == nil {
		return fmt.Errorf("component: host is nil")
	}
	if c == nil {
		return fmt.Errorf("component: component is nil")
	}
	name := c.Name()
	if name == "" {
		return fmt.Errorf("component: name is required")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.items[name]; exists {
		return fmt.Errorf("component: %q already registered", name)
	}
	h.items[name] = c
	return nil
}

// Get returns a component by name.
func (h *Host) Get(name string) (Component, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.items[name]
	return c, ok
}

// ByCapability returns a snapshot of components exposing cap.
func (h *Host) ByCapability(cap Capability) []Component {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Component, 0, len(h.items))
	for _, c := range h.items {
		caps := c.Capabilities()
		for i := range caps {
			if caps[i] == cap {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// Names returns a snapshot of component names.
func (h *Host) Names() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	names := make([]string, 0, len(h.items))
	for name := range h.items {
		names = append(names, name)
	}
	return names
}

// StartAll starts all components. Stops on first error.
func (h *Host) StartAll(ctx context.Context) error {
	h.mu.RLock()
	items := make([]Component, 0, len(h.items))
	for _, c := range h.items {
		items = append(items, c)
	}
	h.mu.RUnlock()

	for _, c := range items {
		if err := c.Start(ctx); err != nil {
			return fmt.Errorf("component: start %q: %w", c.Name(), err)
		}
	}
	return nil
}

// CloseAll closes all registered components and returns the first error.
func (h *Host) CloseAll() error {
	h.mu.RLock()
	items := make([]Component, 0, len(h.items))
	for _, c := range h.items {
		items = append(items, c)
	}
	h.mu.RUnlock()

	var firstErr error
	for _, c := range items {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("component: close %q: %w", c.Name(), err)
		}
	}
	return firstErr
}
