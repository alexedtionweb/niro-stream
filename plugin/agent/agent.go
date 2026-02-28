package agent

import (
	"context"
	"fmt"
	"strings"
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

// WithSystemPrompt sets a system prompt sent on every turn.
// This is the primary way to give an agent its persona and instructions.
func WithSystemPrompt(prompt string) Option {
	return func(rt *Runtime) { rt.systemPrompt = prompt }
}

// WithOptions sets default generation parameters (temperature, max tokens, etc.)
// applied to every turn. Per-request overrides are not yet exposed — use
// RunRequest for full control.
func WithOptions(opts ryn.Options) Option {
	return func(rt *Runtime) { rt.defaultOptions = opts }
}

// WithMiddleware wraps the provider with a middleware function.
// Middlewares are applied in the order they are registered, innermost first:
//
//	agent.New(base,
//		WithMiddleware(retry),   // applied 1st — wraps base
//		WithMiddleware(cache),   // applied 2nd — wraps retry(base)
//	)
//
// Components applied via WithComponent are then layered on top of all
// WithMiddleware wrappers.
func WithMiddleware(fn func(ryn.Provider) ryn.Provider) Option {
	return func(rt *Runtime) {
		if fn != nil {
			rt.provider = fn(rt.provider)
		}
	}
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
	provider       ryn.Provider
	model          string
	systemPrompt   string
	defaultOptions ryn.Options
	memory         Memory
	peers          map[string]Peer
	components     []Component
	host           *component.Host
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

// Provider returns the current (possibly middleware-wrapped) provider.
// Useful for extensions and tests that need to inspect the provider chain.
func (rt *Runtime) Provider() ryn.Provider {
	if rt == nil {
		return nil
	}
	return rt.provider
}

// SystemPrompt returns the system prompt configured on this runtime.
func (rt *Runtime) SystemPrompt() string {
	if rt == nil {
		return ""
	}
	return rt.systemPrompt
}

// loadMessages returns the full message slice for a turn: history + the new
// user message. It is the single place that touches session memory on load.
func (rt *Runtime) loadMessages(ctx context.Context, sessionID, input string) ([]ryn.Message, error) {
	messages := make([]ryn.Message, 0, 8)
	if rt.memory != nil && sessionID != "" {
		history, err := rt.memory.Load(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		messages = append(messages, history...)
	}
	messages = append(messages, ryn.UserText(input))
	return messages, nil
}

// Run executes one conversational turn and blocks until the full response is
// collected. Use RunStream when you need to emit tokens as they arrive.
func (rt *Runtime) Run(ctx context.Context, sessionID string, input string) (TurnResult, error) {
	if rt == nil || rt.provider == nil {
		return TurnResult{}, fmt.Errorf("agent: runtime/provider is nil")
	}

	messages, err := rt.loadMessages(ctx, sessionID, input)
	if err != nil {
		return TurnResult{}, err
	}

	req := &ryn.Request{
		Model:        rt.model,
		SystemPrompt: rt.systemPrompt,
		Messages:     messages,
		Options:      rt.defaultOptions,
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

// RunStream executes a conversational turn and returns a live stream of frames.
// This is the preferred path for real-time output (voice, chat UI, code streaming).
//
// Memory is saved automatically once the stream is fully consumed by the caller.
// If the context is canceled mid-stream (e.g. user disconnects), the partial
// response is discarded and memory is NOT updated — the turn is treated as if it
// never happened.
//
// Example — stream to an HTTP response:
//
//	stream, err := rt.RunStream(ctx, sessionID, userInput)
//	for stream.Next(ctx) {
//		fmt.Fprint(w, stream.Frame().Text)
//	}
func (rt *Runtime) RunStream(ctx context.Context, sessionID string, input string) (*ryn.Stream, error) {
	if rt == nil || rt.provider == nil {
		return nil, fmt.Errorf("agent: runtime/provider is nil")
	}

	messages, err := rt.loadMessages(ctx, sessionID, input)
	if err != nil {
		return nil, err
	}

	req := &ryn.Request{
		Model:        rt.model,
		SystemPrompt: rt.systemPrompt,
		Messages:     messages,
		Options:      rt.defaultOptions,
	}

	src, err := rt.provider.Generate(ctx, req)
	if err != nil {
		return nil, err
	}

	out, emitter := ryn.NewStream(32)
	go func() {
		defer emitter.Close()
		var buf strings.Builder
		for src.Next(ctx) {
			f := src.Frame()
			if f.Kind == ryn.KindText {
				buf.WriteString(f.Text)
			}
			if err := emitter.Emit(ctx, f); err != nil {
				// Consumer stopped reading (context canceled or closed).
				// Do not save partial history.
				return
			}
		}
		if err := src.Err(); err != nil {
			emitter.Error(err)
			return
		}
		if resp := src.Response(); resp != nil {
			emitter.SetResponse(resp)
		}
		// Forward accumulated usage so callers see correct token counts.
		// KindUsage frames are silently consumed by src.Next() and must be
		// re-emitted explicitly — same pattern as tools.ToolLoop.run().
		if u := src.Usage(); u.InputTokens > 0 || u.OutputTokens > 0 || u.TotalTokens > 0 {
			uCopy := u
			_ = emitter.Emit(ctx, ryn.UsageFrame(&uCopy))
		}
		// Save memory only after the full response has been delivered.
		if rt.memory != nil && sessionID != "" {
			nextHistory := append(messages, ryn.AssistantText(buf.String()))
			// Ignore save errors — the stream has already been delivered.
			_ = rt.memory.Save(ctx, sessionID, nextHistory)
		}
	}()
	return out, nil
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
