package agent

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/alexedtionweb/niro-stream"
)

// AgentDefinition describes an agent declaratively.
type AgentDefinition struct {
	Name  string `json:"name"`
	Desc  string `json:"description,omitempty"`
	Steps []Step `json:"steps"`
}

// Step represents a single orchestration step.
type Step struct {
	// Type: "llm" | "tool" | "peer"
	Type string `json:"type"`

	// Input is the step input. If empty, the previous step's output is used.
	Input string `json:"input,omitempty"`

	// OutputVar optionally names this step's output (stored for reference; not used in input resolution).
	OutputVar string `json:"output_var,omitempty"`

	// Tool step fields
	ToolName string          `json:"tool_name,omitempty"`
	ToolArgs json.RawMessage `json:"tool_args,omitempty"`

	// Peer step fields
	PeerName string `json:"peer_name,omitempty"`
}

// LoadAgentDefinition loads a JSON agent definition from disk.
func LoadAgentDefinition(path string) (*AgentDefinition, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load agent def: %w", err)
	}
	var d AgentDefinition
	if err := niro.JSONUnmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("parse agent def: %w", err)
	}
	return &d, nil
}
