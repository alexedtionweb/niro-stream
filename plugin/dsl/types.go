package dsl

import "encoding/json"

// DSLDefinition is the top-level agent definition (tools, schemas, agents).
// Parse from JSON with ParseDSL; validate with Validate; compile with Compile.
type DSLDefinition struct {
	// Tools is map of tool name -> raw spec. Each spec can have "type" (e.g. "http", "code")
	// and type-specific fields. Omit "type" or use "code" for custom handlers from ToolHandlers.
	Tools   map[string]json.RawMessage `json:"tools"`
	Schemas map[string]json.RawMessage `json:"schemas"`
	Agents  map[string]AgentConfig     `json:"agents"`
}

// toolSpecCode is the shape for the default "code" type (args + handler from registry).
type toolSpecCode struct {
	Type        string                 `json:"type,omitempty"`
	Description string                 `json:"description,omitempty"`
	Args        map[string]any `json:"args"`
}

// AgentConfig is the configuration for one agent in the DSL.
type AgentConfig struct {
	Model       string       `json:"model"`
	ModelConfig ModelConfig  `json:"model_config"`
	Prompt      PromptConfig `json:"prompt"`
	Tools      []AgentToolRef `json:"tools"`
	ToolChoice string        `json:"tool_choice"`
	Output     OutputConfig   `json:"output"`
	Limits      LimitsConfig  `json:"limits"`
}

// ModelConfig maps to niro.Options (temperature, max_tokens, thinking_budget).
type ModelConfig struct {
	Temperature    *float64 `json:"temperature"`
	MaxTokens      int      `json:"max_tokens"`
	ThinkingBudget *int     `json:"thinking_budget"`
}

// PromptConfig holds the system prompt; Template is a Go text/template string (RunContext as data).
type PromptConfig struct {
	Template string `json:"template"`
}

// AgentToolRef references a tool by name with optional when/unless (expr expressions).
type AgentToolRef struct {
	Name   string `json:"name"`
	When   string `json:"when,omitempty"`
	Unless string `json:"unless,omitempty"`
}

// OutputConfig describes allowed output types, modalities, and schema refs.
type OutputConfig struct {
	Allow      []string          `json:"allow"`
	Modalities []string          `json:"modalities"`
	Schemas    map[string]string  `json:"schemas"`
	Stream     bool              `json:"stream"`
}

// LimitsConfig enforces max tool calls and optional max parallel tools.
type LimitsConfig struct {
	MaxToolCalls     int `json:"max_tool_calls"`
	MaxParallelTools int `json:"max_parallel_tools"`
}
