package platform

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestRuntimeRun_DeterministicExecutionOrder(t *testing.T) {
	registry := testRegistry()
	builder := staticCandidateBuilder{domains: []string{"billing", "plan"}}
	interpreter := staticInterpreter{
		out: InterpreterOutput{
			Tasks: []TaskRequest{
				{DomainID: "billing", Capability: "pay_invoice"},
				{DomainID: "plan", Capability: "change_plan"},
			},
			PrimaryTaskIndex: 0,
		},
	}

	rt, err := New(registry, builder, interpreter, TemplateRenderer{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	rt.WithGates(staticGate{
		name: "identity",
		result: GateResult{
			ForcedTasks: []TaskRequest{{DomainID: "auth", Capability: "identify_user"}},
			Signals:     []string{"identity_required"},
		},
	})
	rt.WithTool("identify_tool", staticTool{variables: map[string]any{"message": "identity checked"}})
	rt.WithTool("pay_tool", staticTool{variables: map[string]any{"message": "invoice paid"}})
	rt.WithTool("plan_tool", staticTool{variables: map[string]any{"message": "plan changed"}})

	out, err := rt.Run(context.Background(), TurnInput{
		SessionID: "s-1",
		Message:   "quero pagar a conta e mudar o plano",
		State:     "start",
		Context:   map[string]any{},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got := traceNames(out.Trace)
	want := []string{
		traceUserInput,
		traceSystemGateEvaluation,
		traceForcedInternalSteps,
		traceCandidateSelection,
		traceLLMInterpretation,
		traceTaskQueueUpdate,
		traceStateMachineTransition,
		traceScenarioSelection,
		traceToolExecution,
		traceResponseRendering,
		traceStateMachineTransition,
		traceScenarioSelection,
		traceToolExecution,
		traceResponseRendering,
		traceStateMachineTransition,
		traceScenarioSelection,
		traceToolExecution,
		traceResponseRendering,
		traceUserOutput,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trace order mismatch\n got: %v\nwant: %v", got, want)
	}

	if out.State != "plan_changed" {
		t.Fatalf("final state = %q, want %q", out.State, "plan_changed")
	}
	if len(out.Queue) != 3 {
		t.Fatalf("queue length = %d, want 3", len(out.Queue))
	}
	if out.Queue[0].DomainID != "auth" || out.Queue[1].DomainID != "billing" || out.Queue[2].DomainID != "plan" {
		t.Fatalf("unexpected queue order: %+v", out.Queue)
	}
	for _, task := range out.Queue {
		if task.Status != TaskCompleted {
			t.Fatalf("task %s status = %s, want completed", task.ID, task.Status)
		}
	}
}

func TestRuntimeRun_SequentialTaskResponses(t *testing.T) {
	registry := testRegistry()
	rt, err := New(
		registry,
		staticCandidateBuilder{domains: []string{"billing", "plan"}},
		staticInterpreter{
			out: InterpreterOutput{
				Tasks: []TaskRequest{
					{DomainID: "billing", Capability: "pay_invoice"},
					{DomainID: "plan", Capability: "change_plan"},
				},
			},
		},
		TemplateRenderer{},
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	rt.WithTool("pay_tool", staticTool{variables: map[string]any{"message": "invoice paid"}})
	rt.WithTool("plan_tool", staticTool{variables: map[string]any{"message": "plan changed"}})

	out, err := rt.Run(context.Background(), TurnInput{
		SessionID: "s-2",
		Message:   "pagar e trocar plano",
		State:     "ready",
		Context:   map[string]any{},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(out.Responses) != 2 {
		t.Fatalf("responses = %d, want 2", len(out.Responses))
	}
	if !strings.Contains(out.Responses[0].Text, "invoice paid") {
		t.Fatalf("first response = %q", out.Responses[0].Text)
	}
	if !strings.Contains(out.Responses[1].Text, "plan changed") {
		t.Fatalf("second response = %q", out.Responses[1].Text)
	}
}

func TestValidateRegistry_AliasConflict(t *testing.T) {
	registry := testRegistry()
	billing := registry.Domains["billing"]
	billing.Aliases = append(billing.Aliases, "conta")
	registry.Domains["billing"] = billing
	auth := registry.Domains["auth"]
	auth.Aliases = append(auth.Aliases, "conta")
	registry.Domains["auth"] = auth

	err := ValidateRegistry(registry)
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("ValidateRegistry() error = %v, want alias conflict", err)
	}
}

func traceNames(trace Trace) []string {
	out := make([]string, 0, len(trace.Steps))
	for _, step := range trace.Steps {
		out = append(out, step.Name)
	}
	return out
}

type staticCandidateBuilder struct {
	domains []string
}

func (s staticCandidateBuilder) Build(_ context.Context, _ TurnInput, _ *Registry) ([]string, error) {
	return append([]string(nil), s.domains...), nil
}

type staticInterpreter struct {
	out InterpreterOutput
}

func (s staticInterpreter) Interpret(_ context.Context, _ InterpreterInput) (InterpreterOutput, error) {
	return s.out, nil
}

type staticGate struct {
	name   string
	result GateResult
}

func (s staticGate) Name() string { return s.name }

func (s staticGate) Evaluate(_ context.Context, _ TurnInput) (GateResult, error) {
	return s.result, nil
}

type staticTool struct {
	variables map[string]any
}

func (s staticTool) Execute(_ context.Context, _ ToolCall) (ToolResult, error) {
	return ToolResult{Variables: cloneMap(s.variables)}, nil
}

func testRegistry() *Registry {
	return &Registry{
		InitialState: "start",
		Templates: map[string]Template{
			"ack":    {ID: "ack", Approved: true, Text: "{{.message}}"},
			"result": {ID: "result", Approved: true, Text: "{{.message}} in {{.state}}"},
		},
		StateMachine: StateMachine{
			States: map[string]StateDefinition{
				"start":        {AllowedDomains: []string{"auth"}, Transitions: []string{"ready"}},
				"ready":        {AllowedDomains: []string{"billing", "plan"}, Transitions: []string{"invoice_paid", "plan_changed"}},
				"invoice_paid": {AllowedDomains: []string{"plan"}, Transitions: []string{"plan_changed"}},
				"plan_changed": {AllowedDomains: []string{}, Transitions: []string{"plan_changed"}},
			},
		},
		Domains: map[string]Domain{
			"auth": {
				ID:               "auth",
				Aliases:          []string{"identificar", "login"},
				Examples:         []string{"preciso me identificar"},
				NegativeExamples: []string{"quero pagar fatura"},
				Capabilities: map[string]Capability{
					"identify_user": {
						ID: "identify_user",
						Scenarios: []Scenario{
							{
								ID:               "identify_from_start",
								AllowedStates:    []string{"start"},
								ToolID:           "identify_tool",
								NextState:        "ready",
								ResponseTemplate: "ack",
							},
						},
					},
				},
			},
			"billing": {
				ID:               "billing",
				Aliases:          []string{"fatura", "pagamento"},
				Examples:         []string{"quero pagar minha conta"},
				NegativeExamples: []string{"quero trocar meu plano"},
				Capabilities: map[string]Capability{
					"pay_invoice": {
						ID: "pay_invoice",
						Scenarios: []Scenario{
							{
								ID:               "pay_ready",
								AllowedStates:    []string{"ready"},
								ToolID:           "pay_tool",
								NextState:        "invoice_paid",
								ResponseTemplate: "result",
							},
						},
					},
				},
				Relationships: []Relationship{{Type: "dependency", Target: "auth"}},
			},
			"plan": {
				ID:               "plan",
				Aliases:          []string{"plano", "upgrade"},
				Examples:         []string{"quero mudar meu plano"},
				NegativeExamples: []string{"quero pagar fatura"},
				Capabilities: map[string]Capability{
					"change_plan": {
						ID: "change_plan",
						Scenarios: []Scenario{
							{
								ID:               "change_after_payment",
								AllowedStates:    []string{"invoice_paid"},
								ToolID:           "plan_tool",
								NextState:        "plan_changed",
								ResponseTemplate: "result",
							},
							{
								ID:               "change_ready",
								AllowedStates:    []string{"ready"},
								ToolID:           "plan_tool",
								NextState:        "plan_changed",
								ResponseTemplate: "result",
							},
						},
					},
				},
				Relationships: []Relationship{{Type: "prerequisite", Target: "auth"}},
			},
		},
	}
}
