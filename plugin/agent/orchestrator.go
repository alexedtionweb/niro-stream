package agent

import (
	"context"
	"fmt"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/tools"
)

// Orchestrator executes a declarative agent definition using an Agent
// and optional Toolset for direct tool invocation.
type Orchestrator struct {
	Runtime *Agent
	Toolset *tools.Toolset
}

// NewOrchestrator creates an orchestrator bound to an agent.
func NewOrchestrator(rt *Agent, ts *tools.Toolset) *Orchestrator {
	return &Orchestrator{Runtime: rt, Toolset: ts}
}

// RunDefinition executes the agent definition sequentially.
// Steps run in order. Input for each step is Step.Input if set, otherwise the
// previous step's output. Context cancellation is respected.
func (o *Orchestrator) RunDefinition(ctx context.Context, sessionID string, def *AgentDefinition) (string, error) {
	if def == nil {
		return "", fmt.Errorf("agent: definition is nil")
	}

	var lastText string

	for i, s := range def.Steps {
		if err := ctx.Err(); err != nil {
			return lastText, err
		}

		input := resolveInput(s, lastText)

		var err error
		switch s.Type {
		case "llm":
			if o.Runtime == nil {
				return "", fmt.Errorf("step %d llm: no runtime available to execute LLM step", i)
			}
			tr, runErr := o.Runtime.Run(ctx, sessionID, input)
			if runErr != nil {
				return "", fmt.Errorf("step %d llm: %w", i, runErr)
			}
			lastText = tr.Text

		case "tool":
			if o.Toolset == nil {
				return "", fmt.Errorf("step %d tool: no toolset available", i)
			}
			call := niro.ToolCall{Name: s.ToolName, Args: s.ToolArgs}
			res, execErr := o.Toolset.ExecuteCall(ctx, call)
			if execErr != nil {
				return "", fmt.Errorf("step %d tool exec: %w", i, execErr)
			}
			lastText = res.Content

		case "peer":
			if o.Runtime == nil {
				return "", fmt.Errorf("step %d peer: runtime missing", i)
			}
			lastText, err = o.Runtime.CallPeer(ctx, s.PeerName, sessionID, input)
			if err != nil {
				return "", fmt.Errorf("step %d peer call: %w", i, err)
			}

		default:
			return "", fmt.Errorf("step %d: unknown type %q", i, s.Type)
		}
	}
	return lastText, nil
}

// resolveInput returns the step input: Step.Input if non-empty, else previous output.
// No template or substitution in the input path.
func resolveInput(s Step, lastText string) string {
	if s.Input != "" {
		return s.Input
	}
	return lastText
}
