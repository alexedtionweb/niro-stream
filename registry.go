package ryn

import (
	"context"
	"fmt"
	"sync"
)

// Registry is a named provider registry that allows looking up
// providers by name at runtime. It is the canonical way to manage
// multiple providers in a production deployment.
//
// Registry is goroutine-safe. Providers can be registered and
// retrieved concurrently from any goroutine.
//
// Usage:
//
//	reg := ryn.NewRegistry()
//	reg.Register("openai", openaiProvider)
//	reg.Register("anthropic", anthropicProvider)
//	reg.Register("fast", ryn.NewCache(opts).Wrap(openaiProvider))
//
//	// Later, at request time:
//	llm, err := reg.Get("openai")
//	stream, err := llm.Generate(ctx, req)
//
//	// Or iterate all:
//	for name, p := range reg.All() { ... }
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry creates an empty provider registry.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]Provider),
	}
}

// Register adds a named provider to the registry.
// Overwrites any existing provider with the same name.
func (r *Registry) Register(name string, p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = p
}

// Get retrieves a provider by name.
// Returns an error if the name is not registered.
func (r *Registry) Get(name string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("ryn: provider %q not registered", name)
	}
	return p, nil
}

// MustGet retrieves a provider by name or panics.
// Use in init() or setup code where missing providers are fatal.
func (r *Registry) MustGet(name string) Provider {
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
// The returned map is a copy — safe to iterate without holding locks.
func (r *Registry) All() map[string]Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snap := make(map[string]Provider, len(r.providers))
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

// Generate is a convenience that looks up a provider by name and
// calls Generate. This allows Registry to be used directly as a
// routing layer.
func (r *Registry) Generate(ctx context.Context, name string, req *Request) (*Stream, error) {
	p, err := r.Get(name)
	if err != nil {
		return nil, err
	}
	return p.Generate(ctx, req)
}

// DefaultRegistry is a process-wide provider registry.
// Use for applications that prefer a global registry pattern.
var DefaultRegistry = NewRegistry()
