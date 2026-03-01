package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/alexedtionweb/niro-stream"
)

// ToolHandler executes a tool with raw JSON args and returns any JSON-serializable value.
// Return string to emit plain text content directly.
type ToolHandler func(ctx context.Context, args json.RawMessage) (any, error)

// ToolDefinition is a high-level tool abstraction.
// It maps to ryn.Request.Tools for providers and supports runtime execution.
type ToolDefinition struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Handler     ToolHandler
}

// NewToolDefinition creates a tool from raw JSON schema bytes.
func NewToolDefinition(name, description string, schema json.RawMessage, handler ToolHandler) (ToolDefinition, error) {
	d := ToolDefinition{
		Name:        name,
		Description: description,
		Schema:      schema,
		Handler:     handler,
	}
	if err := d.Validate(); err != nil {
		return ToolDefinition{}, err
	}
	return d, nil
}

// NewToolDefinitionAny creates a tool using the configured JSON backend.
func NewToolDefinitionAny(name, description string, schema any, handler ToolHandler) (ToolDefinition, error) {
	b, err := ryn.JSONMarshal(schema)
	if err != nil {
		return ToolDefinition{}, err
	}
	return NewToolDefinition(name, description, b, handler)
}

// Validate validates tool definition shape and schema.
// It delegates name format, description, and schema checks to ryn.Tool.Validate
// so that the same cross-provider rules are enforced in one place.
func (d ToolDefinition) Validate() error {
	t := d.ToTool()
	if err := t.Validate(); err != nil {
		return err
	}
	if d.Handler == nil {
		return fmt.Errorf("tool handler is required")
	}
	return nil
}

// ToTool converts definition to provider-facing ryn.Tool.
func (d ToolDefinition) ToTool() ryn.Tool {
	return ryn.Tool{
		Name:        d.Name,
		Description: d.Description,
		Parameters:  d.Schema,
	}
}

// ToolValidationInfo contains validation event data.
type ToolValidationInfo struct {
	Name string
	Args json.RawMessage
	Err  error
}

// ToolExecutionInfo contains execution event data.
type ToolExecutionInfo struct {
	Name   string
	CallID string
	Args   json.RawMessage
	Result ryn.ToolResult
	Err    error
}

// ToolRuntimeHook receives tool validation/execution lifecycle events.
type ToolRuntimeHook interface {
	OnToolValidate(ctx context.Context, info ToolValidationInfo)
	OnToolExecuteStart(ctx context.Context, call ryn.ToolCall)
	OnToolExecuteEnd(ctx context.Context, info ToolExecutionInfo)
}

// --- Human-in-the-loop (HITL) approval ---

// ToolApproval is the outcome of an approval decision.
type ToolApproval struct {
	// Approved must be true for the tool call to proceed.
	Approved bool
	// Reason is fed back to the LLM as the tool result when Approved is false.
	// Provide a clear human-readable message — the model will use it to explain
	// to the user why the action was blocked.
	Reason string
}

// ToolApprover intercepts tool calls before execution for human or
// policy-based review. Implementations block until a decision is made.
// The context carries the deadline — if no decision arrives in time,
// context cancellation is the timeout mechanism.
//
// Example — channel-based human approval:
//
//	type ChannelApprover struct { Requests chan ApprovalRequest }
//	func (a *ChannelApprover) Approve(ctx context.Context, call ryn.ToolCall) (ToolApproval, error) {
//		req := ApprovalRequest{Call: call, Reply: make(chan ToolApproval, 1)}
//		select {
//		case a.Requests <- req:
//		case <-ctx.Done(): return ToolApproval{}, ctx.Err()
//		}
//		select {
//		case dec := <-req.Reply: return dec, nil
//		case <-ctx.Done():       return ToolApproval{}, ctx.Err()
//		}
//	}
type ToolApprover interface {
	Approve(ctx context.Context, call ryn.ToolCall) (ToolApproval, error)
}

// ToolApproverFunc adapts a function to the ToolApprover interface.
type ToolApproverFunc func(ctx context.Context, call ryn.ToolCall) (ToolApproval, error)

func (f ToolApproverFunc) Approve(ctx context.Context, call ryn.ToolCall) (ToolApproval, error) {
	return f(ctx, call)
}

// ApproveAll returns a ToolApprover that approves every call unconditionally.
// Use as a no-op placeholder or in tests.
func ApproveAll() ToolApprover {
	return ToolApproverFunc(func(_ context.Context, _ ryn.ToolCall) (ToolApproval, error) {
		return ToolApproval{Approved: true}, nil
	})
}

// DenyAll returns a ToolApprover that denies every call with the given reason.
// Useful for blocking all tool execution pending a review or during maintenance.
func DenyAll(reason string) ToolApprover {
	if reason == "" {
		reason = "all tool calls are currently denied"
	}
	return ToolApproverFunc(func(_ context.Context, call ryn.ToolCall) (ToolApproval, error) {
		return ToolApproval{Approved: false, Reason: reason}, nil
	})
}

// ErrToolDenied is returned by ExecuteCall when a ToolApprover rejects a call.
// The Reason is returned to the model as the tool result content so the model
// can explain to the user why the action was blocked.
type ErrToolDenied struct {
	CallName string
	Reason   string
}

func (e *ErrToolDenied) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("tool %q not approved: %s", e.CallName, e.Reason)
	}
	return fmt.Sprintf("tool %q not approved", e.CallName)
}

// IsToolDenied reports whether err (or any error in its chain) is an ErrToolDenied.
func IsToolDenied(err error) bool {
	var e *ErrToolDenied
	return errors.As(err, &e)
}

// ToolSchemaValidator validates tool call args against schema.
type ToolSchemaValidator interface {
	Validate(schema json.RawMessage, args json.RawMessage) error
}

type toolSchemaValidatorFunc func(schema json.RawMessage, args json.RawMessage) error

func (f toolSchemaValidatorFunc) Validate(schema json.RawMessage, args json.RawMessage) error {
	return f(schema, args)
}

// validatorHolder wraps a ToolSchemaValidator for storage in atomic.Value.
// Using a wrapper struct ensures atomic.Value always stores the same concrete type.
type validatorHolder struct {
	v ToolSchemaValidator
}

// approverHolder wraps a ToolApprover for lock-free storage in atomic.Value.
// A stored nil (*approverHolder)(nil) is never used; an absent value means
// no approver is set — loaded as nil, skipped with a single nil check.
type approverHolder struct {
	a ToolApprover
}

var globalToolSchemaValidator atomic.Value

func init() {
	globalToolSchemaValidator.Store(&validatorHolder{v: defaultToolSchemaValidator()})
}

// SetToolSchemaValidator sets the global schema validator. Nil resets to default.
func SetToolSchemaValidator(v ToolSchemaValidator) {
	if v == nil {
		globalToolSchemaValidator.Store(&validatorHolder{v: defaultToolSchemaValidator()})
		return
	}
	globalToolSchemaValidator.Store(&validatorHolder{v: v})
}

// CurrentToolSchemaValidator returns active validator.
// The global is always initialised by init(), so this never returns nil.
func CurrentToolSchemaValidator() ToolSchemaValidator {
	return globalToolSchemaValidator.Load().(*validatorHolder).v
}

func defaultToolSchemaValidator() ToolSchemaValidator {
	return toolSchemaValidatorFunc(func(schema json.RawMessage, args json.RawMessage) error {
		if len(args) > 0 && !ryn.JSONValid(args) {
			return fmt.Errorf("tool args are not valid JSON")
		}
		if len(schema) == 0 || len(args) == 0 {
			return nil
		}

		var s map[string]any
		if err := ryn.JSONUnmarshal(schema, &s); err != nil {
			return fmt.Errorf("invalid schema json: %w", err)
		}

		reqRaw, ok := s["required"]
		if !ok {
			return nil
		}
		reqList, ok := reqRaw.([]any)
		if !ok {
			return nil
		}

		var payload map[string]any
		if err := ryn.JSONUnmarshal(args, &payload); err != nil {
			return fmt.Errorf("invalid args json: %w", err)
		}
		for _, item := range reqList {
			k, ok := item.(string)
			if !ok {
				continue
			}
			if _, exists := payload[k]; !exists {
				return fmt.Errorf("missing required field %q", k)
			}
		}
		return nil
	})
}

// Toolset provides a Genkit-like abstraction over tool declaration,
// provider wiring, validation, hooks, and execution.
type Toolset struct {
	mu        sync.RWMutex
	defs      map[string]ToolDefinition
	hooks     []ToolRuntimeHook
	validator ToolSchemaValidator
	// approver is stored in an atomic.Value so ExecuteCall can read it without
	// taking the mutex — a single atomic load + nil check on the hot path.
	// Zero value: no approver set (all calls proceed without review).
	approver atomic.Value // stores *approverHolder
}

// NewToolset creates an empty toolset.
func NewToolset() *Toolset {
	return &Toolset{
		defs:      make(map[string]ToolDefinition),
		validator: CurrentToolSchemaValidator(),
	}
}

// WithValidator overrides schema validator for this toolset.
func (ts *Toolset) WithValidator(v ToolSchemaValidator) *Toolset {
	if ts == nil || v == nil {
		return ts
	}
	ts.mu.Lock()
	ts.validator = v
	ts.mu.Unlock()
	return ts
}

// WithHook appends a runtime hook.
func (ts *Toolset) WithHook(h ToolRuntimeHook) *Toolset {
	if ts == nil || h == nil {
		return ts
	}
	ts.mu.Lock()
	ts.hooks = append(ts.hooks, h)
	ts.mu.Unlock()
	return ts
}

// Register adds or replaces a tool definition.
func (ts *Toolset) Register(def ToolDefinition) error {
	if err := def.Validate(); err != nil {
		return err
	}
	ts.mu.Lock()
	ts.defs[def.Name] = def
	ts.mu.Unlock()
	return nil
}

// MustRegister panics if registration fails.
func (ts *Toolset) MustRegister(def ToolDefinition) *Toolset {
	if err := ts.Register(def); err != nil {
		panic(err)
	}
	return ts
}

// Remove unregisters a tool by name. Returns true if the tool existed.
func (ts *Toolset) Remove(name string) bool {
	ts.mu.Lock()
	_, ok := ts.defs[name]
	if ok {
		delete(ts.defs, name)
	}
	ts.mu.Unlock()
	return ok
}

// Get returns the ToolDefinition registered under name.
func (ts *Toolset) Get(name string) (ToolDefinition, bool) {
	return ts.get(name)
}

// Names returns all registered tool names in sorted order.
func (ts *Toolset) Names() []string {
	ts.mu.RLock()
	names := make([]string, 0, len(ts.defs))
	for name := range ts.defs {
		names = append(names, name)
	}
	ts.mu.RUnlock()
	sort.Strings(names)
	return names
}

// Len returns the number of registered tools.
func (ts *Toolset) Len() int {
	ts.mu.RLock()
	n := len(ts.defs)
	ts.mu.RUnlock()
	return n
}

// WithApprover sets a human-in-the-loop approval gate for this toolset.
// The approver is consulted before every tool handler execution.
// A nil approver removes any previously set approver.
// Stored atomically — safe to call concurrently with ExecuteCall.
func (ts *Toolset) WithApprover(a ToolApprover) *Toolset {
	if ts == nil {
		return ts
	}
	if a == nil {
		// Store zero value to clear any previously set approver.
		ts.approver.Store((*approverHolder)(nil))
	} else {
		ts.approver.Store(&approverHolder{a: a})
	}
	return ts
}

// Tools returns provider-facing tool list sorted by name for deterministic ordering.
func (ts *Toolset) Tools() []ryn.Tool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	names := make([]string, 0, len(ts.defs))
	for name := range ts.defs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ryn.Tool, 0, len(names))
	for _, name := range names {
		out = append(out, ts.defs[name].ToTool())
	}
	return out
}

// Apply returns a shallow copy of req with toolset tools attached.
func (ts *Toolset) Apply(req *ryn.Request) *ryn.Request {
	if req == nil {
		return nil
	}
	r := *req
	r.Tools = ts.Tools()
	return &r
}

// Execute implements ToolExecutor.
func (ts *Toolset) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	res, err := ts.ExecuteCall(ctx, ryn.ToolCall{Name: name, Args: args})
	if err != nil {
		return "", err
	}
	return res.Content, nil
}

// ExecuteCall validates args, invokes handler, and returns ryn.ToolResult.
func (ts *Toolset) ExecuteCall(ctx context.Context, call ryn.ToolCall) (ryn.ToolResult, error) {
	def, ok := ts.get(call.Name)
	if !ok {
		return ryn.ToolResult{CallID: call.ID, Content: "tool not found", IsError: true}, ryn.NewErrorf(ryn.ErrCodeInvalidRequest, "tool %q not found", call.Name)
	}

	valErr := ts.validator.Validate(def.Schema, call.Args)
	ts.emitValidate(ctx, ToolValidationInfo{Name: call.Name, Args: call.Args, Err: valErr})
	if valErr != nil {
		return ryn.ToolResult{CallID: call.ID, Content: valErr.Error(), IsError: true}, valErr
	}

	// HITL: approval gate — runs after validation but before execution.
	// The approver blocks until a human or policy makes a decision.
	// Context cancellation is the timeout mechanism.
	// Hot path (no approver): single atomic load + nil check, ~1 ns, no allocation.
	var approver ToolApprover
	if h, _ := ts.approver.Load().(*approverHolder); h != nil {
		approver = h.a
	}
	if approver != nil {
		decision, approveErr := approver.Approve(ctx, call)
		if approveErr != nil {
			return ryn.ToolResult{
				CallID:  call.ID,
				Content: "approval error: " + approveErr.Error(),
				IsError: true,
			}, approveErr
		}
		if !decision.Approved {
			reason := decision.Reason
			if reason == "" {
				reason = "tool call was not approved"
			}
			denied := &ErrToolDenied{CallName: call.Name, Reason: reason}
			return ryn.ToolResult{CallID: call.ID, Content: denied.Error(), IsError: true}, denied
		}
	}

	ts.emitExecStart(ctx, call)
	out, err := def.Handler(ctx, call.Args)
	if err != nil {
		info := ToolExecutionInfo{
			Name:   call.Name,
			CallID: call.ID,
			Args:   call.Args,
			Result: ryn.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true},
			Err:    err,
		}
		ts.emitExecEnd(ctx, info)
		return info.Result, err
	}

	content, convErr := normalizeToolOutput(out)
	if convErr != nil {
		info := ToolExecutionInfo{
			Name:   call.Name,
			CallID: call.ID,
			Args:   call.Args,
			Result: ryn.ToolResult{CallID: call.ID, Content: convErr.Error(), IsError: true},
			Err:    convErr,
		}
		ts.emitExecEnd(ctx, info)
		return info.Result, convErr
	}

	res := ryn.ToolResult{CallID: call.ID, Content: content, IsError: false}
	ts.emitExecEnd(ctx, ToolExecutionInfo{Name: call.Name, CallID: call.ID, Args: call.Args, Result: res})
	return res, nil
}

func (ts *Toolset) get(name string) (ToolDefinition, bool) {
	ts.mu.RLock()
	def, ok := ts.defs[name]
	ts.mu.RUnlock()
	return def, ok
}

func (ts *Toolset) emitValidate(ctx context.Context, info ToolValidationInfo) {
	ts.mu.RLock()
	hooks := append([]ToolRuntimeHook(nil), ts.hooks...)
	ts.mu.RUnlock()
	for _, h := range hooks {
		h.OnToolValidate(ctx, info)
	}
}

func (ts *Toolset) emitExecStart(ctx context.Context, call ryn.ToolCall) {
	ts.mu.RLock()
	hooks := append([]ToolRuntimeHook(nil), ts.hooks...)
	ts.mu.RUnlock()
	for _, h := range hooks {
		h.OnToolExecuteStart(ctx, call)
	}
}

func (ts *Toolset) emitExecEnd(ctx context.Context, info ToolExecutionInfo) {
	ts.mu.RLock()
	hooks := append([]ToolRuntimeHook(nil), ts.hooks...)
	ts.mu.RUnlock()
	for _, h := range hooks {
		h.OnToolExecuteEnd(ctx, info)
	}
}

func normalizeToolOutput(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", nil
	case string:
		return x, nil
	case []byte:
		return string(x), nil
	case json.RawMessage:
		return string(x), nil
	default:
		b, err := ryn.JSONMarshal(x)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

// ToolingProvider is a smart provider wrapper that applies declared tools
// and executes tool loops with validation + hooks.
type ToolingProvider struct {
	base ryn.Provider
	set  *Toolset
	opts ToolStreamOptions
}

// NewToolingProvider creates a provider wrapper that uses a Toolset.
func NewToolingProvider(base ryn.Provider, set *Toolset, opts ToolStreamOptions) *ToolingProvider {
	if set == nil {
		set = NewToolset()
	}
	if opts.MaxRounds == 0 && opts.StreamBuffer == 0 && opts.ToolTimeout == 0 {
		opts = DefaultToolStreamOptions()
	}
	return &ToolingProvider{base: base, set: set, opts: opts}
}

// Generate applies tool definitions and executes tool loop.
func (p *ToolingProvider) Generate(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
	if p == nil || p.base == nil {
		return nil, ryn.NewError(ryn.ErrCodeInvalidRequest, "base provider is nil")
	}
	if req == nil {
		return nil, ryn.NewError(ryn.ErrCodeInvalidRequest, "request is nil")
	}
	r := p.set.Apply(req)
	loop := NewToolLoopWithOptions(p.set, p.opts)
	return loop.GenerateWithTools(ctx, p.base, r)
}
