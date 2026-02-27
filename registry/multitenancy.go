package registry

import (
	"context"
	"fmt"

	"ryn.dev/ryn"
)

// ClientSelector resolves a client/provider name for a request.
// Return an empty string to defer to other resolution sources.
type ClientSelector func(ctx context.Context, req *ryn.Request) (string, error)

// RequestMutator modifies a cloned request before dispatching to a
// selected client/provider.
type RequestMutator func(ctx context.Context, req *ryn.Request) error

// MultiTenantOption configures MultiTenantProvider.
type MultiTenantOption func(*multiTenantConfig)

type multiTenantConfig struct {
	defaultClient string
	selector      ClientSelector
	mutators      map[string]RequestMutator
}

// WithDefaultClient sets the fallback client used when request/context do not
// specify a client.
func WithDefaultClient(name string) MultiTenantOption {
	return func(c *multiTenantConfig) {
		c.defaultClient = name
	}
}

// WithClientSelector sets a custom runtime selector.
func WithClientSelector(selector ClientSelector) MultiTenantOption {
	return func(c *multiTenantConfig) {
		c.selector = selector
	}
}

// WithClientMutator registers a per-client request mutator.
func WithClientMutator(client string, mutator RequestMutator) MultiTenantOption {
	return func(c *multiTenantConfig) {
		if c.mutators == nil {
			c.mutators = make(map[string]RequestMutator)
		}
		c.mutators[client] = mutator
	}
}

// MultiTenantProvider routes requests to a runtime-selected provider.
//
// Selection precedence:
//  1. Custom selector (WithClientSelector)
//  2. req.Client
//  3. Client stored in context (WithClient)
//  4. WithDefaultClient
//  5. Single registered provider (if registry len == 1)
//
// It clones the incoming request before applying mutators, so original requests
// remain immutable from the caller perspective.
type MultiTenantProvider struct {
	registry       *Registry
	defaultClient  string
	selector       ClientSelector
	clientMutators map[string]RequestMutator
}

var _ ryn.Provider = (*MultiTenantProvider)(nil)

// NewMultiTenantProvider creates a provider router backed by a Registry.
func NewMultiTenantProvider(reg *Registry, opts ...MultiTenantOption) *MultiTenantProvider {
	cfg := &multiTenantConfig{mutators: make(map[string]RequestMutator)}
	for _, o := range opts {
		o(cfg)
	}
	return &MultiTenantProvider{
		registry:       reg,
		defaultClient:  cfg.defaultClient,
		selector:       cfg.selector,
		clientMutators: cfg.mutators,
	}
}

// Generate resolves a runtime client and forwards the request.
func (p *MultiTenantProvider) Generate(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
	if p == nil || p.registry == nil {
		return nil, fmt.Errorf("ryn: multi-tenant provider requires a registry")
	}
	if req == nil {
		return nil, fmt.Errorf("ryn: request cannot be nil")
	}

	client, err := p.resolveClient(ctx, req)
	if err != nil {
		return nil, err
	}

	provider, err := p.registry.Get(client)
	if err != nil {
		return nil, err
	}

	cloned := cloneRequest(req)
	if mutator, ok := p.clientMutators[client]; ok && mutator != nil {
		if err := mutator(ctx, cloned); err != nil {
			return nil, fmt.Errorf("ryn: client mutator %q: %w", client, err)
		}
	}

	return provider.Generate(ctx, cloned)
}

func (p *MultiTenantProvider) resolveClient(ctx context.Context, req *ryn.Request) (string, error) {
	if p.selector != nil {
		name, err := p.selector(ctx, req)
		if err != nil {
			return "", err
		}
		if name != "" {
			return name, nil
		}
	}

	if req.Client != "" {
		return req.Client, nil
	}
	if name, ok := ClientFromContext(ctx); ok && name != "" {
		return name, nil
	}
	if p.defaultClient != "" {
		return p.defaultClient, nil
	}
	if p.registry.Len() == 1 {
		for name := range p.registry.All() {
			return name, nil
		}
	}
	return "", fmt.Errorf("ryn: no client selected (set request.Client, context client, or default)")
}

type clientContextKey struct{}

// WithClient stores the runtime client/provider name in context.
func WithClient(ctx context.Context, client string) context.Context {
	return context.WithValue(ctx, clientContextKey{}, client)
}

// ClientFromContext returns the runtime client/provider name stored in context.
func ClientFromContext(ctx context.Context) (string, bool) {
	v := ctx.Value(clientContextKey{})
	s, ok := v.(string)
	return s, ok
}

func cloneRequest(req *ryn.Request) *ryn.Request {
	if req == nil {
		return nil
	}
	out := *req

	if len(req.Messages) > 0 {
		out.Messages = make([]ryn.Message, len(req.Messages))
		for i, m := range req.Messages {
			out.Messages[i] = cloneMessage(m)
		}
	}
	if len(req.Tools) > 0 {
		out.Tools = append([]ryn.Tool(nil), req.Tools...)
	}
	if len(req.ResponseSchema) > 0 {
		out.ResponseSchema = append([]byte(nil), req.ResponseSchema...)
	}
	if len(req.Options.Stop) > 0 {
		out.Options.Stop = append([]string(nil), req.Options.Stop...)
	}
	return &out
}

func cloneMessage(in ryn.Message) ryn.Message {
	m := in
	if len(in.Parts) > 0 {
		m.Parts = make([]ryn.Part, len(in.Parts))
		copy(m.Parts, in.Parts)
		for i := range m.Parts {
			if len(m.Parts[i].Data) > 0 {
				m.Parts[i].Data = append([]byte(nil), m.Parts[i].Data...)
			}
			if m.Parts[i].Tool != nil {
				tc := *m.Parts[i].Tool
				if len(tc.Args) > 0 {
					tc.Args = append([]byte(nil), tc.Args...)
				}
				m.Parts[i].Tool = &tc
			}
			if m.Parts[i].Result != nil {
				tr := *m.Parts[i].Result
				m.Parts[i].Result = &tr
			}
		}
	}
	return m
}
