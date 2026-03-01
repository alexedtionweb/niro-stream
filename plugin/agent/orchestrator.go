package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/tools"
)

// Orchestrator executes a declarative agent definition using a Runtime
// and optional Toolset for direct tool invocation.
type Orchestrator struct {
	Runtime *Runtime
	Toolset *tools.Toolset
}

// NewOrchestrator creates an orchestrator bound to an agent runtime.
func NewOrchestrator(rt *Runtime, ts *tools.Toolset) *Orchestrator {
	return &Orchestrator{Runtime: rt, Toolset: ts}
}

// RunDefinition executes the agent definition sequentially.
// Steps run in order; each step's output is implicitly available to the next
// as {{.LastText}} or via a named variable set by OutputVar.
//
// Context cancellation is respected between steps and inside sleep steps.
func (o *Orchestrator) RunDefinition(ctx context.Context, sessionID string, def *AgentDefinition) (string, error) {
	if def == nil {
		return "", fmt.Errorf("agent: definition is nil")
	}

	var (
		lastText string
		vars     = make(map[string]string)
	)

	for i, s := range def.Steps {
		// Abort early if the context was canceled between steps.
		if err := ctx.Err(); err != nil {
			return lastText, err
		}

		var err error
		switch s.Type {
		case "llm":
			input := resolveInput(s, lastText, vars)
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
			input := resolveInput(s, lastText, vars)
			lastText, err = o.Runtime.CallPeer(ctx, s.PeerName, sessionID, input)
			if err != nil {
				return "", fmt.Errorf("step %d peer call: %w", i, err)
			}

		case "sleep":
			d := time.Duration(s.SleepSeconds) * time.Second
			if d > 0 {
				select {
				case <-ctx.Done():
					return lastText, ctx.Err()
				case <-time.After(d):
				}
			}

		default:
			return "", fmt.Errorf("step %d: unknown type %q", i, s.Type)
		}

		// Capture output into a named variable if requested.
		if s.OutputVar != "" {
			vars[s.OutputVar] = lastText
		}
	}
	return lastText, nil
}

// resolveInput returns the effective input for a step using priority order:
//  1. s.Input (explicit, supports {{.LastText}} and {{.VarName}} substitution)
//  2. s.Messages[0] (legacy field)
//  3. lastText (implicit output chaining from the previous step)
func resolveInput(s Step, lastText string, vars map[string]string) string {
	input := s.Input
	if input == "" && len(s.Messages) > 0 {
		input = s.Messages[0]
	}
	if input == "" {
		input = lastText
	}
	// Variable substitution: {{.LastText}} and {{.VarName}}.
	input = strings.ReplaceAll(input, "{{.LastText}}", lastText)
	for k, v := range vars {
		input = strings.ReplaceAll(input, "{{."+k+"}}", v)
	}
	return input
}
