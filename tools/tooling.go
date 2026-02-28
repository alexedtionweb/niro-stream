package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"ryn.dev/ryn"
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
func (d ToolDefinition) Validate() error {
	if d.Name == "" {
		return fmt.Errorf("tool name is required")
	}
	if d.Description == "" {
		return fmt.Errorf("tool description is required")
	}
	if len(d.Schema) > 0 && !ryn.JSONValid(d.Schema) {
		return fmt.Errorf("tool schema is not valid JSON")
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
func CurrentToolSchemaValidator() ToolSchemaValidator {
	if v := globalToolSchemaValidator.Load(); v != nil {
		return v.(*validatorHolder).v
	}
	d := defaultToolSchemaValidator()
	globalToolSchemaValidator.Store(&validatorHolder{v: d})
	return d
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

// Tools returns provider-facing tool list.
func (ts *Toolset) Tools() []ryn.Tool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]ryn.Tool, 0, len(ts.defs))
	for _, d := range ts.defs {
		out = append(out, d.ToTool())
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
	if opts.MaxRounds == 0 && opts.StreamBuffer == 0 && !opts.Parallel && !opts.EmitToolResults && opts.ToolTimeout == 0 {
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
