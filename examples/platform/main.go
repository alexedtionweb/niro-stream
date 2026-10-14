package main

import (
	"context"
	"fmt"

	"github.com/alexedtionweb/niro-stream/platform"
)

type candidateBuilder struct{}

func (candidateBuilder) Build(_ context.Context, _ platform.TurnInput, _ *platform.Registry) ([]string, error) {
	return []string{"billing"}, nil
}

type interpreter struct{}

func (interpreter) Interpret(_ context.Context, _ platform.InterpreterInput) (platform.InterpreterOutput, error) {
	return platform.InterpreterOutput{
		Tasks: []platform.TaskRequest{
			{DomainID: "billing", Capability: "pay_invoice"},
		},
	}, nil
}

type tool struct{}

func (tool) Execute(_ context.Context, call platform.ToolCall) (platform.ToolResult, error) {
	return platform.ToolResult{
		Variables: map[string]any{"message": "invoice paid"},
	}, nil
}

func main() {
	registry := &platform.Registry{
		InitialState: "ready",
		Templates: map[string]platform.Template{
			"result": {ID: "result", Approved: true, Text: "{{.message}}"},
		},
		StateMachine: platform.StateMachine{
			States: map[string]platform.StateDefinition{
				"ready": {AllowedDomains: []string{"billing"}, Transitions: []string{"done"}},
				"done":  {Transitions: []string{"done"}},
			},
		},
		Domains: map[string]platform.Domain{
			"billing": {
				ID:               "billing",
				Aliases:          []string{"fatura"},
				Examples:         []string{"quero pagar minha conta"},
				NegativeExamples: []string{"quero mudar meu plano"},
				Capabilities: map[string]platform.Capability{
					"pay_invoice": {
						ID: "pay_invoice",
						Scenarios: []platform.Scenario{
							{
								ID:               "pay_ready",
								AllowedStates:    []string{"ready"},
								ToolID:           "pay_tool",
								NextState:        "done",
								ResponseTemplate: "result",
							},
						},
					},
				},
			},
		},
	}

	rt, err := platform.New(registry, candidateBuilder{}, interpreter{}, platform.TemplateRenderer{})
	if err != nil {
		panic(err)
	}
	rt.WithTool("pay_tool", tool{})

	out, err := rt.Run(context.Background(), platform.TurnInput{
		SessionID: "s-1",
		Message:   "quero pagar minha conta",
		State:     "ready",
	})
	if err != nil {
		panic(err)
	}

	fmt.Println(out.Responses[0].Text)
	fmt.Println("final state:", out.State)
}
