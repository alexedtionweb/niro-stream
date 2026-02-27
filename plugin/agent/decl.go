package agent

import (
	"encoding/json"
	"fmt"
	"os"
)

// AgentDefinition describes an agent declaratively.
type AgentDefinition struct {
	Name  string `json:"name"`
	Desc  string `json:"description,omitempty"`
	Steps []Step `json:"steps"`
}

// Step represents a single orchestration step.
type Step struct {
	// Type: "llm" | "tool" | "peer" | "sleep"
	Type string `json:"type"`

	// LLM step fields
	Messages []string `json:"messages,omitempty"`

	// Tool step fields
	ToolName string          `json:"tool_name,omitempty"`
	ToolArgs json.RawMessage `json:"tool_args,omitempty"`

	// Peer step fields
	PeerName string `json:"peer_name,omitempty"`

	// Sleep (seconds)
	SleepSeconds int `json:"sleep_seconds,omitempty"`
}

// LoadAgentDefinition loads a JSON agent definition from disk.
func LoadAgentDefinition(path string) (*AgentDefinition, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load agent def: %w", err)
	}
	var d AgentDefinition
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("parse agent def: %w", err)
	}
	return &d, nil
}
