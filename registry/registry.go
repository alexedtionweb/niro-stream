// Package registry provides named provider registration and routing.
package registry

import (
	"context"
	"fmt"
	"sync"

	"ryn.dev/ryn"
)

// Registry is a named provider registry that allows looking up
// providers by name at runtime. It is the canonical way to manage
// multiple providers in a production deployment.
//
// Registry is goroutine-safe.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]ryn.Provider
}

// New creates an empty provider registry.
func New() *Registry {
	return &Registry{
		providers: make(map[string]ryn.Provider),
	}
}

// Register adds a named provider to the registry.
// Overwrites any existing provider with the same name.
func (r *Registry) Register(name string, p ryn.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = p
}

// Get retrieves a provider by name.
// Returns an error if the name is not registered.
func (r *Registry) Get(name string) (ryn.Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("ryn: provider %q not registered", name)
	}
	return p, nil
}

// MustGet retrieves a provider by name or panics.
func (r *Registry) MustGet(name string) ryn.Provider {
	p, err := r.Get(name)
	if err != nil {
		panic(err)
	}
	return p
}

// Has returns true if a provider with the given name is registered.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.providers[name]
	return ok
}

// Remove unregisters a provider by name. No-op if not found.
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.providers, name)
}

// All returns a snapshot of all registered providers.
func (r *Registry) All() map[string]ryn.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snap := make(map[string]ryn.Provider, len(r.providers))
	for k, v := range r.providers {
		snap[k] = v
	}
	return snap
}

// Names returns the names of all registered providers.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for k := range r.providers {
		names = append(names, k)
	}
	return names
}

// Len returns the number of registered providers.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.providers)
}

// Generate is a convenience that looks up a provider by name and calls Generate.
func (r *Registry) Generate(ctx context.Context, name string, req *ryn.Request) (*ryn.Stream, error) {
	p, err := r.Get(name)
	if err != nil {
		return nil, err
	}
	return p.Generate(ctx, req)
}

// Default is a process-wide provider registry.
// Use for applications that prefer a global registry pattern.
var Default = New()
