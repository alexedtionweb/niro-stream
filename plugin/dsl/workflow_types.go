package dsl

// WorkflowDefinition is the top-level workflow definition (separate JSON from agent def).
// Can represent a single workflow or a map of named workflows.
type WorkflowDefinition struct {
	// Single-workflow form: name, description, type, agents
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type,omitempty"` // "fan" | "race" | "sequence"
	Agents      []string `json:"agents,omitempty"`

	// Multi-workflow form: workflows map
	Workflows map[string]WorkflowNode `json:"workflows,omitempty"`
}

// WorkflowNode describes one workflow: type and list of agent names.
// For a single-agent entry (e.g. chat), use Agent instead of Agents to avoid the array pattern.
type WorkflowNode struct {
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"`   // "fan" | "race" | "sequence" | "fan_then"
	Agent       string   `json:"agent,omitempty"`  // single agent by name (entry point)
	Agents      []string `json:"agents,omitempty"` // agent names (for fan/sequence/fan_then)
	ThenAgent   string   `json:"then_agent,omitempty"` // for fan_then: agent that synthesizes parallel results
}
