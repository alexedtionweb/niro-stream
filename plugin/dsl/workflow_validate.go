package dsl

import (
	"github.com/alexedtionweb/niro-stream"
)

// ValidateWorkflow checks that the workflow references only existing agent names.
// agentNames is the set of agent names from the compiled agent definition (e.g. keys of NiroDefinition.Agents).
func ValidateWorkflow(wf *WorkflowDefinition, agentNames map[string]struct{}) error {
	if wf == nil {
		return niro.NewError(niro.ErrCodeInvalidRequest, "dsl: workflow definition is nil")
	}
	if agentNames == nil {
		agentNames = make(map[string]struct{})
	}

	checkAgents := func(agents []string, ctx string) error {
		for _, name := range agents {
			if name == "" {
				return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: workflow %s: empty agent name", ctx)
			}
			if _, ok := agentNames[name]; !ok {
				return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: workflow %s: agent %q not found in agent definition", ctx, name)
			}
		}
		return nil
	}

	if len(wf.Workflows) > 0 {
		for wname, node := range wf.Workflows {
			if node.Type != "fan" && node.Type != "race" && node.Type != "sequence" && node.Type != "fan_then" {
				return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: workflow %q: type must be fan, race, sequence, or fan_then", wname)
			}
			agents := node.Agents
			if node.Agent != "" {
				agents = []string{node.Agent}
			}
			if len(agents) == 0 {
				return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: workflow %q: agent or agents required", wname)
			}
			if node.Type == "fan_then" {
				if node.ThenAgent == "" {
					return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: workflow %q: fan_then requires then_agent", wname)
				}
				if _, ok := agentNames[node.ThenAgent]; !ok {
					return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: workflow %q: then_agent %q not found", wname, node.ThenAgent)
				}
			}
			if err := checkAgents(agents, wname); err != nil {
				return err
			}
		}
		return nil
	}

	if wf.Type != "fan" && wf.Type != "race" && wf.Type != "sequence" {
		return niro.NewError(niro.ErrCodeInvalidRequest, "dsl: workflow: type must be fan, race, or sequence")
	}
	if len(wf.Agents) == 0 {
		return niro.NewError(niro.ErrCodeInvalidRequest, "dsl: workflow: agents list is empty")
	}
	return checkAgents(wf.Agents, wf.Name)
}
