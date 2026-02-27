package agent

import (
	"context"
	"fmt"
	"time"

	"ryn.dev/ryn"
)

// Orchestrator executes a declarative agent definition using a Runtime
// and optional Toolset for direct tool invocation.
type Orchestrator struct {
	Runtime *Runtime
	Toolset *ryn.Toolset
}

// NewOrchestrator creates an orchestrator bound to an agent runtime.
func NewOrchestrator(rt *Runtime, ts *ryn.Toolset) *Orchestrator {
	return &Orchestrator{Runtime: rt, Toolset: ts}
}

// RunDefinition executes the agent definition sequentially.
// It returns the final output text and error if any step fails.
func (o *Orchestrator) RunDefinition(ctx context.Context, sessionID string, def *AgentDefinition) (string, error) {
	if def == nil {
		return "", fmt.Errorf("agent: definition is nil")
	}
	var lastText string
	for i, s := range def.Steps {
		switch s.Type {
		case "llm":
			// Prepare input (join messages in simple form)
			var input string
			if len(s.Messages) > 0 {
				input = s.Messages[0]
			}
			// Prefer using the agent Runtime (applies memory, peers, etc.)
			if o.Runtime != nil {
				tr, err := o.Runtime.Run(ctx, sessionID, input)
				if err != nil {
					return "", fmt.Errorf("step %d llm: %w", i, err)
				}
				lastText = tr.Text
				continue
			}
			return "", fmt.Errorf("step %d llm: no runtime available to execute LLM step", i)

		case "tool":
			if o.Toolset == nil {
				return "", fmt.Errorf("step %d tool: no toolset available", i)
			}
			// Execute tool directly
			call := ryn.ToolCall{Name: s.ToolName, Args: s.ToolArgs}
			res, err := o.Toolset.ExecuteCall(ctx, call)
			if err != nil {
				return "", fmt.Errorf("step %d tool exec: %w", i, err)
			}
			lastText = res.Content

		case "peer":
			if o.Runtime == nil {
				return "", fmt.Errorf("step %d peer: runtime missing", i)
			}
			out, err := o.Runtime.CallPeer(ctx, s.PeerName, sessionID, s.Messages[0])
			if err != nil {
				return "", fmt.Errorf("step %d peer call: %w", i, err)
			}
			lastText = out

		case "sleep":
			time.Sleep(time.Duration(s.SleepSeconds) * time.Second)

		default:
			return "", fmt.Errorf("step %d: unknown type %q", i, s.Type)
		}
	}
	return lastText, nil
}
