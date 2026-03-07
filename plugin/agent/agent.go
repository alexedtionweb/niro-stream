package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/component"
)

// Memory is the storage primitive for conversation history by session ID.
// Implement it to inject any backend (SQL, NoSQL, Redis, file); the runtime only calls Load and Save.
//
//   - Load: return full history (oldest to newest). Optional: implement [BoundedLoader] so the runtime can call LoadLast for better performance.
//   - Save: replace stored history for that session. Should be idempotent (same input → same state) for fault-tolerant retries.
//
// Use WithMemory(mem). For stateless use nil or [StatelessMemory]. For fault tolerance the runtime retries Load/Save with backoff (see [WithMemoryRetry]).
type Memory interface {
	Load(ctx context.Context, sessionID string) ([]niro.Message, error)
	Save(ctx context.Context, sessionID string, history []niro.Message) error
}

// BoundedLoader is an optional extension of [Memory]. When implemented, the runtime may call LoadLast
// instead of Load when the [HistoryPolicy] reports a message cap (e.g. [SlidingWindow](20)), so the backend
// can fetch only the last N messages (e.g. SQL ORDER BY id DESC LIMIT N, Redis LRANGE -N -1).
// Fallback: if LoadLast is not implemented or maxMessages <= 0, the runtime uses Load and trims in memory.
type BoundedLoader interface {
	Memory
	// LoadLast returns up to maxMessages most recent messages (oldest to newest).
	// If maxMessages <= 0, return the same as Load (full history).
	LoadLast(ctx context.Context, sessionID string, maxMessages int) ([]niro.Message, error)
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
func WithOptions(opts niro.Options) Option {
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
func WithMiddleware(fn func(niro.Provider) niro.Provider) Option {
	return func(rt *Runtime) {
		if fn != nil {
			rt.provider = fn(rt.provider)
		}
	}
}

// WithMemory attaches the history store. Implement [Memory] with SQL, NoSQL, Redis, etc.
// Omit or pass nil for stateless; or use [StatelessMemory]. See [WithMemoryRetry] for fault tolerance.
func WithMemory(mem Memory) Option {
	return func(rt *Runtime) { rt.memory = mem }
}

// WithMemoryRetry configures retries for Load and Save (transient failures). attempts is max tries (1 = no retry); initialBackoff is the first delay.
// Default when unset: 3 attempts, 50ms initial backoff, exponential backoff. Set attempts <= 1 to disable retry.
func WithMemoryRetry(attempts int, initialBackoff time.Duration) Option {
	return func(rt *Runtime) {
		rt.memoryRetryAttempts = attempts
		rt.memoryRetryBackoff = initialBackoff
	}
}

// WithHistoryPolicy sets how conversation history is trimmed before each request (e.g. SlidingWindow, NoHistory).
// Memory still stores full history; the policy only limits what is sent to the model.
func WithHistoryPolicy(policy HistoryPolicy) Option {
	return func(rt *Runtime) { rt.historyPolicy = policy }
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

// Runtime is an optional agent layer built on top of niro core primitives.
//
// Core remains agent-agnostic; this module composes provider + memory + peers
// and plugin components to execute conversational turns.
type Runtime struct {
	provider              niro.Provider
	model                 string
	systemPrompt          string
	defaultOptions        niro.Options
	memory                Memory
	historyPolicy         HistoryPolicy
	memoryRetryAttempts   int           // max attempts for Load/Save (1 = no retry)
	memoryRetryBackoff    time.Duration // first backoff duration
	peers          map[string]Peer
	components     []Component
	host           *component.Host
}

// TurnResult contains response text and metadata.
type TurnResult struct {
	Text     string
	Usage    niro.Usage
	Response *niro.ResponseMeta
}

// New creates an agent runtime plugin.
func New(provider niro.Provider, opts ...Option) (*Runtime, error) {
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
	if rt.memoryRetryAttempts <= 0 {
		rt.memoryRetryAttempts = 3
	}
	if rt.memoryRetryBackoff <= 0 {
		rt.memoryRetryBackoff = 50 * time.Millisecond
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
func (rt *Runtime) Provider() niro.Provider {
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

// loadWithRetry runs fn up to memoryRetryAttempts with exponential backoff. Returns the last error if all fail.
func (rt *Runtime) loadWithRetry(ctx context.Context, fn func() ([]niro.Message, error)) ([]niro.Message, error) {
	attempts := rt.memoryRetryAttempts
	if attempts <= 1 {
		return fn()
	}
	backoff := rt.memoryRetryBackoff
	var lastErr error
	for i := 0; i < attempts; i++ {
		out, err := fn()
		if err == nil {
			return out, nil
		}
		lastErr = err
		if i < attempts-1 {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
			backoff *= 2
		}
	}
	return nil, lastErr
}

// saveWithRetry runs Save up to memoryRetryAttempts with exponential backoff. Returns the last error if all fail.
func (rt *Runtime) saveWithRetry(ctx context.Context, sessionID string, history []niro.Message) error {
	if rt.memory == nil {
		return nil
	}
	attempts := rt.memoryRetryAttempts
	if attempts <= 1 {
		return rt.memory.Save(ctx, sessionID, history)
	}
	backoff := rt.memoryRetryBackoff
	var lastErr error
	for i := 0; i < attempts; i++ {
		err := rt.memory.Save(ctx, sessionID, history)
		if err == nil {
			return nil
		}
		lastErr = err
		if i < attempts-1 {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			backoff *= 2
		}
	}
	return lastErr
}

// loadMessages returns the message slice for a turn: history (optionally trimmed by HistoryPolicy) + the new user message.
// If memory implements [BoundedLoader] and historyPolicy implements [BoundedHistoryPolicy] with MaxMessages() > 0,
// the runtime calls LoadLast for better performance; otherwise it uses Load and trims in memory.
func (rt *Runtime) loadMessages(ctx context.Context, sessionID, input string) ([]niro.Message, error) {
	messages := make([]niro.Message, 0, 8)
	if rt.memory != nil && sessionID != "" {
		var history []niro.Message
		var err error
		if bl, ok := rt.memory.(BoundedLoader); ok && rt.historyPolicy != nil {
			if bp, ok := rt.historyPolicy.(BoundedHistoryPolicy); ok && bp.MaxMessages() > 0 {
				history, err = rt.loadWithRetry(ctx, func() ([]niro.Message, error) {
					return bl.LoadLast(ctx, sessionID, bp.MaxMessages())
				})
			} else {
				history, err = rt.loadWithRetry(ctx, func() ([]niro.Message, error) {
					return rt.memory.Load(ctx, sessionID)
				})
			}
		} else {
			history, err = rt.loadWithRetry(ctx, func() ([]niro.Message, error) {
				return rt.memory.Load(ctx, sessionID)
			})
		}
		if err != nil {
			return nil, err
		}
		if rt.historyPolicy != nil {
			history = rt.historyPolicy.TrimForRequest(history)
		}
		messages = append(messages, history...)
	}
	messages = append(messages, niro.UserText(input))
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

	req := &niro.Request{
		Model:        rt.model,
		SystemPrompt: rt.systemPrompt,
		Messages:     messages,
		Options:      rt.defaultOptions,
	}

	stream, err := rt.provider.Generate(ctx, req)
	if err != nil {
		return TurnResult{}, err
	}
	text, err := niro.CollectText(ctx, stream)
	if err != nil {
		return TurnResult{}, err
	}

	res := TurnResult{
		Text:     text,
		Usage:    stream.Usage(),
		Response: stream.Response(),
	}

	if rt.memory != nil && sessionID != "" {
		nextHistory := append(messages, niro.AssistantText(text))
		if p, ok := rt.historyPolicy.(HistorySavePolicy); ok && p != nil {
			nextHistory = p.TrimForSave(nextHistory)
		}
		if err := rt.saveWithRetry(ctx, sessionID, nextHistory); err != nil {
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
func (rt *Runtime) RunStream(ctx context.Context, sessionID string, input string) (*niro.Stream, error) {
	if rt == nil || rt.provider == nil {
		return nil, fmt.Errorf("agent: runtime/provider is nil")
	}

	messages, err := rt.loadMessages(ctx, sessionID, input)
	if err != nil {
		return nil, err
	}

	req := &niro.Request{
		Model:        rt.model,
		SystemPrompt: rt.systemPrompt,
		Messages:     messages,
		Options:      rt.defaultOptions,
	}

	src, err := rt.provider.Generate(ctx, req)
	if err != nil {
		return nil, err
	}

	out, emitter := niro.NewStream(32)
	go func() {
		defer emitter.Close()
		var buf strings.Builder
		for src.Next(ctx) {
			f := src.Frame()
			if f.Kind == niro.KindText {
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
			_ = emitter.Emit(ctx, niro.UsageFrame(&uCopy))
		}
		// Save memory only after the full response has been delivered.
		if rt.memory != nil && sessionID != "" {
			nextHistory := append(messages, niro.AssistantText(buf.String()))
			if p, ok := rt.historyPolicy.(HistorySavePolicy); ok && p != nil {
				nextHistory = p.TrimForSave(nextHistory)
			}
			if err := rt.saveWithRetry(ctx, sessionID, nextHistory); err != nil {
				emitter.Error(err)
			}
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

// StatelessMemory implements [Memory] with no persistence: Load always returns nil,
// Save is a no-op. Use when you want explicit "no history" (e.g. WithMemory(agent.StatelessMemory())).
var StatelessMemory Memory = statelessMemory{}

type statelessMemory struct{}

func (statelessMemory) Load(context.Context, string) ([]niro.Message, error) { return nil, nil }
func (statelessMemory) Save(context.Context, string, []niro.Message) error   { return nil }

// InMemoryMemory is a simple in-process memory store (map by session ID).
// Use for local/dev; for production inject SQL, NoSQL, Redis, etc. via [Memory].
type InMemoryMemory struct {
	mu       sync.RWMutex
	sessions map[string][]niro.Message
}

// NewInMemoryMemory creates a memory store for local/dev usage.
func NewInMemoryMemory() *InMemoryMemory {
	return &InMemoryMemory{sessions: make(map[string][]niro.Message)}
}

func (m *InMemoryMemory) Load(ctx context.Context, sessionID string) ([]niro.Message, error) {
	_ = ctx
	m.mu.RLock()
	defer m.mu.RUnlock()
	h := m.sessions[sessionID]
	out := make([]niro.Message, len(h))
	copy(out, h)
	return out, nil
}

func (m *InMemoryMemory) Save(ctx context.Context, sessionID string, history []niro.Message) error {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]niro.Message, len(history))
	copy(out, history)
	m.sessions[sessionID] = out
	return nil
}

// LoadLast implements [BoundedLoader]: returns up to maxMessages most recent messages (oldest to newest).
// If maxMessages <= 0, returns full history like Load.
func (m *InMemoryMemory) LoadLast(ctx context.Context, sessionID string, maxMessages int) ([]niro.Message, error) {
	_ = ctx
	m.mu.RLock()
	defer m.mu.RUnlock()
	h := m.sessions[sessionID]
	if maxMessages <= 0 || len(h) <= maxMessages {
		out := make([]niro.Message, len(h))
		copy(out, h)
		return out, nil
	}
	start := len(h) - maxMessages
	out := make([]niro.Message, maxMessages)
	copy(out, h[start:])
	return out, nil
}
