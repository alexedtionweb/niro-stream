package agent

import (
	"context"
	"fmt"
	"sync"

	"ryn.dev/ryn"
	"ryn.dev/ryn/component"
)

// Memory stores conversation state by session ID.
// Implementations can be in-memory, DB-backed, Redis, MCP-backed, etc.
type Memory interface {
	Load(ctx context.Context, sessionID string) ([]ryn.Message, error)
	Save(ctx context.Context, sessionID string, history []ryn.Message) error
}

// MCPMemory is an optional extension for memory providers backed by MCP.
type MCPMemory interface {
	Memory
	Namespace() string
}

// Peer enables agent-to-agent calls.
type Peer interface {
	Name() string
	Ask(ctx context.Context, sessionID string, input string) (string, error)
}

// Component is an agent plugin component mounted into Runtime.
type Component interface {
	component.Component
	Apply(rt *Runtime) error
}

// Option configures Runtime.
type Option func(*Runtime)

// WithModel sets the runtime default model.
func WithModel(model string) Option {
	return func(rt *Runtime) { rt.model = model }
}

// WithMemory attaches session memory.
func WithMemory(mem Memory) Option {
	return func(rt *Runtime) { rt.memory = mem }
}

// WithPeer registers an agent peer.
func WithPeer(peer Peer) Option {
	return func(rt *Runtime) {
		if peer == nil {
			return
		}
		if rt.peers == nil {
			rt.peers = make(map[string]Peer)
		}
		rt.peers[peer.Name()] = peer
	}
}

// WithComponent registers a plugin component.
func WithComponent(c Component) Option {
	return func(rt *Runtime) {
		if c == nil {
			return
		}
		rt.components = append(rt.components, c)
	}
}

// Runtime is an optional agent layer built on top of ryn core primitives.
//
// Core remains agent-agnostic; this module composes provider + memory + peers
// and plugin components to execute conversational turns.
type Runtime struct {
	provider   ryn.Provider
	model      string
	memory     Memory
	peers      map[string]Peer
	components []Component
	host       *component.Host
}

// TurnResult contains response text and metadata.
type TurnResult struct {
	Text     string
	Usage    ryn.Usage
	Response *ryn.ResponseMeta
}

// New creates an agent runtime plugin.
func New(provider ryn.Provider, opts ...Option) (*Runtime, error) {
	if provider == nil {
		return nil, fmt.Errorf("agent: provider is nil")
	}
	rt := &Runtime{
		provider: provider,
		peers:    make(map[string]Peer),
		host:     component.NewHost(),
	}
	for _, o := range opts {
		o(rt)
	}

	for _, c := range rt.components {
		if err := rt.host.Register(c); err != nil {
			return nil, err
		}
		if err := c.Apply(rt); err != nil {
			return nil, fmt.Errorf("agent: apply component %q: %w", c.Name(), err)
		}
	}

	return rt, nil
}

// Start starts all registered components.
func (rt *Runtime) Start(ctx context.Context) error {
	if rt == nil {
		return fmt.Errorf("agent: runtime is nil")
	}
	return rt.host.StartAll(ctx)
}

// Close closes all registered components.
func (rt *Runtime) Close() error {
	if rt == nil {
		return nil
	}
	return rt.host.CloseAll()
}

// Run executes one conversational turn.
func (rt *Runtime) Run(ctx context.Context, sessionID string, input string) (TurnResult, error) {
	if rt == nil || rt.provider == nil {
		return TurnResult{}, fmt.Errorf("agent: runtime/provider is nil")
	}

	messages := make([]ryn.Message, 0, 8)
	if rt.memory != nil && sessionID != "" {
		history, err := rt.memory.Load(ctx, sessionID)
		if err != nil {
			return TurnResult{}, err
		}
		messages = append(messages, history...)
	}
	messages = append(messages, ryn.UserText(input))

	req := &ryn.Request{
		Model:    rt.model,
		Messages: messages,
	}

	stream, err := rt.provider.Generate(ctx, req)
	if err != nil {
		return TurnResult{}, err
	}
	text, err := ryn.CollectText(ctx, stream)
	if err != nil {
		return TurnResult{}, err
	}

	res := TurnResult{
		Text:     text,
		Usage:    stream.Usage(),
		Response: stream.Response(),
	}

	if rt.memory != nil && sessionID != "" {
		nextHistory := append(messages, ryn.AssistantText(text))
		if err := rt.memory.Save(ctx, sessionID, nextHistory); err != nil {
			return TurnResult{}, err
		}
	}

	return res, nil
}

// CallPeer executes an agent-to-agent call.
func (rt *Runtime) CallPeer(ctx context.Context, peerName string, sessionID string, input string) (string, error) {
	if rt == nil {
		return "", fmt.Errorf("agent: runtime is nil")
	}
	peer, ok := rt.peers[peerName]
	if !ok {
		return "", fmt.Errorf("agent: peer %q not found", peerName)
	}
	return peer.Ask(ctx, sessionID, input)
}

// InMemoryMemory is a simple in-process memory plugin.
type InMemoryMemory struct {
	mu       sync.RWMutex
	sessions map[string][]ryn.Message
}

// NewInMemoryMemory creates a memory store for local/dev usage.
func NewInMemoryMemory() *InMemoryMemory {
	return &InMemoryMemory{sessions: make(map[string][]ryn.Message)}
}

func (m *InMemoryMemory) Load(ctx context.Context, sessionID string) ([]ryn.Message, error) {
	_ = ctx
	m.mu.RLock()
	defer m.mu.RUnlock()
	h := m.sessions[sessionID]
	out := make([]ryn.Message, len(h))
	copy(out, h)
	return out, nil
}

func (m *InMemoryMemory) Save(ctx context.Context, sessionID string, history []ryn.Message) error {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ryn.Message, len(history))
	copy(out, history)
	m.sessions[sessionID] = out
	return nil
}
