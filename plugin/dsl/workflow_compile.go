package dsl

import (
	"context"

	"github.com/alexedtionweb/niro-stream"
)

// CompiledWorkflow is a runnable workflow (fan, race, sequence, fan_then) that uses a Runner.
type CompiledWorkflow struct {
	Type      string   // "fan" | "race" | "sequence" | "fan_then"
	Agents    []string // agent names (for fan_then: parallel agents)
	ThenAgent string   // for fan_then: synthesizer agent
	runner    *Runner
}

// CompileWorkflow compiles a workflow definition into a runnable, given the set of valid agent names.
// Typically call after compiling the agent definition so agentNames = keys of NiroDefinition.Agents.
func (wf *WorkflowDefinition) CompileWorkflow(agentNames map[string]struct{}) (map[string]*CompiledWorkflow, error) {
	if err := ValidateWorkflow(wf, agentNames); err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "validate workflow", err)
	}
	out := make(map[string]*CompiledWorkflow)

	if len(wf.Workflows) > 0 {
		for name, node := range wf.Workflows {
			out[name] = &CompiledWorkflow{Type: node.Type, Agents: node.Agents, ThenAgent: node.ThenAgent}
		}
		return out, nil
	}

	out[wf.Name] = &CompiledWorkflow{Type: wf.Type, Agents: wf.Agents}
	return out, nil
}

// Run executes the workflow with the given Runner and RunContext.
// The Runner must be set via BindRunner before calling Run.
func (c *CompiledWorkflow) Run(ctx context.Context, runCtx *RunContext, sessionID string) (*niro.Stream, string, niro.Usage, error) {
	if c.runner == nil {
		return nil, "", niro.Usage{}, niro.NewError(niro.ErrCodeInvalidRequest, "dsl: workflow not bound to runner; call BindRunner first")
	}
	switch c.Type {
	case "fan":
		return c.runner.Fan(ctx, runCtx, sessionID, c.Agents...), "", niro.Usage{}, nil
	case "fan_then":
		return c.runner.FanThen(ctx, runCtx, sessionID, c.Agents, c.ThenAgent)
	case "race":
		text, usage, err := c.runner.Race(ctx, runCtx, sessionID, c.Agents...)
		return nil, text, usage, err
	case "sequence":
		stream, err := c.runner.Sequence(ctx, runCtx, sessionID, c.Agents...)
		return stream, "", niro.Usage{}, err
	default:
		return nil, "", niro.Usage{}, niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: unknown workflow type %q", c.Type)
	}
}

// BindRunner attaches a Runner to this workflow so Run can execute.
func (c *CompiledWorkflow) BindRunner(r *Runner) {
	c.runner = r
}
