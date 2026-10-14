package platform

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskActive    TaskStatus = "active"
	TaskCompleted TaskStatus = "completed"
	TaskCancelled TaskStatus = "cancelled"
)

type TurnInput struct {
	SessionID string
	Message   string
	State     string
	Context   map[string]any
}

type TurnOutput struct {
	SessionID string
	State     string
	Queue     []Task
	Responses []RenderedResponse
	Trace     Trace
}

type RenderedResponse struct {
	TaskID      string
	TemplateID  string
	Text        string
	Variables   map[string]any
	ToolSignals []string
}

type Trace struct {
	Steps []TraceStep
}

type TraceStep struct {
	Name    string
	Details map[string]any
}

type Task struct {
	ID         string
	DomainID   string
	Capability string
	Status     TaskStatus
}

type Runtime struct {
	registry         *Registry
	gates            []Gate
	candidateBuilder CandidateBuilder
	interpreter      Interpreter
	tools            map[string]Tool
	renderer         Renderer
	idFactory        func(prefix string, index int) string
}

type Registry struct {
	InitialState string
	Domains      map[string]Domain
	Templates    map[string]Template
	StateMachine StateMachine
}

type Domain struct {
	ID               string
	Aliases          []string
	Examples         []string
	NegativeExamples []string
	Capabilities     map[string]Capability
	Relationships    []Relationship
}

type Relationship struct {
	Type   string
	Target string
}

type Capability struct {
	ID        string
	Scenarios []Scenario
}

type Scenario struct {
	ID               string
	AllowedStates    []string
	RequiredContext  map[string]string
	ToolID           string
	NextState        string
	ResponseTemplate string
}

type Template struct {
	ID       string
	Locale   string
	Text     string
	Approved bool
}

type StateMachine struct {
	States map[string]StateDefinition
}

type StateDefinition struct {
	AllowedDomains []string
	Transitions    []string
}

type Gate interface {
	Name() string
	Evaluate(ctx context.Context, input TurnInput) (GateResult, error)
}

type GateResult struct {
	Blocked     bool
	ForcedTasks []TaskRequest
	Signals     []string
}

type CandidateBuilder interface {
	Build(ctx context.Context, input TurnInput, registry *Registry) ([]string, error)
}

type Interpreter interface {
	Interpret(ctx context.Context, input InterpreterInput) (InterpreterOutput, error)
}

type InterpreterInput struct {
	Message          string
	CandidateDomains []string
	State            string
	Context          map[string]any
}

type InterpreterOutput struct {
	Tasks            []TaskRequest
	PrimaryTaskIndex int
	FillerTemplateID string
}

type TaskRequest struct {
	DomainID   string
	Capability string
}

type Tool interface {
	Execute(ctx context.Context, call ToolCall) (ToolResult, error)
}

type ToolCall struct {
	SessionID string
	State     string
	Task      Task
	Input     TurnInput
	Scenario  Scenario
}

type ToolResult struct {
	Variables map[string]any
	Signals   []string
}

type Renderer interface {
	Render(t Template, vars map[string]any) (string, error)
}

func New(registry *Registry, candidateBuilder CandidateBuilder, interpreter Interpreter, renderer Renderer) (*Runtime, error) {
	if registry == nil {
		return nil, fmt.Errorf("platform: registry is nil")
	}
	if candidateBuilder == nil {
		return nil, fmt.Errorf("platform: candidate builder is nil")
	}
	if interpreter == nil {
		return nil, fmt.Errorf("platform: interpreter is nil")
	}
	if renderer == nil {
		return nil, fmt.Errorf("platform: renderer is nil")
	}
	if err := ValidateRegistry(registry); err != nil {
		return nil, err
	}
	return &Runtime{
		registry:         registry,
		candidateBuilder: candidateBuilder,
		interpreter:      interpreter,
		renderer:         renderer,
		tools:            make(map[string]Tool),
		idFactory: func(prefix string, index int) string {
			return fmt.Sprintf("%s-%02d", prefix, index+1)
		},
	}, nil
}

func (r *Runtime) WithGates(gates ...Gate) *Runtime {
	r.gates = append([]Gate(nil), gates...)
	return r
}

func (r *Runtime) WithTool(id string, tool Tool) *Runtime {
	if tool == nil {
		return r
	}
	r.tools[id] = tool
	return r
}

func (r *Runtime) WithIDFactory(fn func(prefix string, index int) string) *Runtime {
	if fn != nil {
		r.idFactory = fn
	}
	return r
}

func normalizeStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func contains(list []string, needle string) bool {
	for _, item := range list {
		if item == needle {
			return true
		}
	}
	return false
}

func normalizeToken(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
