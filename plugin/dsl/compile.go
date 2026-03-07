package dsl

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/tools"
)

// CompileOptions configures compilation of the agent definition.
type CompileOptions struct {
	// ToolHandlers maps tool name to handler for "code" type tools. Used when a tool
	// has no "type" or type "code" (args only, handler from here).
	ToolHandlers map[string]tools.ToolHandler
	// ToolTypes maps type name to builder (e.g. "http" -> HTTPToolType). Merged with
	// default registry (RegisterToolType). Option entries override defaults.
	ToolTypes map[string]ToolTypeBuilder
}

// NiroDefinition is the compiled agent definition: per-agent config and optional Toolset.
type NiroDefinition struct {
	// Agents holds compiled config per agent name.
	Agents map[string]*CompiledAgentConfig
	// Toolset is the shared toolset from DSL tools + optional handlers.
	Toolset *tools.Toolset
}

// CompiledAgentConfig is the compiled config for one agent (used by Runner to build Request).
type CompiledAgentConfig struct {
	Model          string
	Options        niro.Options
	PromptTemplate string
	ToolRefs       []AgentToolRef
	ToolChoice     niro.ToolChoice
	Limits         LimitsConfig
	Output         OutputConfig
}

// Compile converts a validated DSLDefinition into NiroDefinition (per-agent config + Toolset).
func (def *DSLDefinition) Compile(opts CompileOptions) (*NiroDefinition, error) {
	if def == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "dsl: compile: definition is nil")
	}
	ts, err := compileTools(def, opts)
	if err != nil {
		return nil, err
	}
	agents, err := compileAgents(def)
	if err != nil {
		return nil, err
	}
	return &NiroDefinition{Agents: agents, Toolset: ts}, nil
}

func compileTools(def *DSLDefinition, opts CompileOptions) (*tools.Toolset, error) {
	out := tools.NewToolset()
	for name, spec := range def.Tools {
		var codeSpec toolSpecCode
		_ = niro.JSONUnmarshal(spec, &codeSpec)
		typeName := codeSpec.Type
		if typeName == "" {
			typeName = "code"
		}
		if builder := getToolType(typeName, opts); builder != nil {
			td, err := builder.Build(name, spec)
			if err != nil {
				return nil, niro.WrapErrorf(niro.ErrCodeInvalidRequest, "dsl: tool %q (type %q)", err, name, typeName)
			}
			if err := out.Register(td); err != nil {
				return nil, niro.WrapErrorf(niro.ErrCodeInvalidRequest, "dsl: register tool %q", err, name)
			}
			continue
		}
		schema, err := argsToJSONSchema(codeSpec.Args)
		if err != nil {
			return nil, niro.WrapErrorf(niro.ErrCodeInvalidRequest, "dsl: tool %q", err, name)
		}
		desc := codeSpec.Description
		if desc == "" {
			desc = "DSL tool " + name
		}
		handler := opts.ToolHandlers[name]
		if handler == nil {
			handler = func(context.Context, json.RawMessage) (any, error) {
				return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: no handler registered for tool %q", name)
			}
		}
		td, err := tools.NewToolDefinition(name, desc, schema, handler)
		if err != nil {
			return nil, niro.WrapErrorf(niro.ErrCodeInvalidRequest, "dsl: tool %q", err, name)
		}
		if err := out.Register(td); err != nil {
			return nil, niro.WrapErrorf(niro.ErrCodeInvalidRequest, "dsl: register tool %q", err, name)
		}
	}
	return out, nil
}

func compileAgents(def *DSLDefinition) (map[string]*CompiledAgentConfig, error) {
	out := make(map[string]*CompiledAgentConfig)
	for name, cfg := range def.Agents {
		compiled := &CompiledAgentConfig{
			Model:          cfg.Model,
			PromptTemplate: cfg.Prompt.Template,
			ToolRefs:       cfg.Tools,
			Limits:         cfg.Limits,
			Output:         cfg.Output,
		}
		compiled.Options = niro.Options{
			MaxTokens:      cfg.ModelConfig.MaxTokens,
			Temperature:    cfg.ModelConfig.Temperature,
			ThinkingBudget: cfg.ModelConfig.ThinkingBudget,
		}
		compiled.ToolChoice = parseToolChoice(cfg.ToolChoice)
		out[name] = compiled
	}
	return out, nil
}

// parseToolChoice maps DSL tool_choice string to niro.ToolChoice.
func parseToolChoice(s string) niro.ToolChoice {
	switch s {
	case "none":
		return niro.ToolChoiceNone
	case "required":
		return niro.ToolChoiceRequired
	case "auto", "":
		return niro.ToolChoiceAuto
	}
	if strings.HasPrefix(s, niro.ToolChoiceFuncPrefix) {
		return niro.ToolChoiceFunc(strings.TrimPrefix(s, niro.ToolChoiceFuncPrefix))
	}
	return niro.ToolChoiceAuto
}

// argsToJSONSchema converts DSL args to JSON Schema. Each arg value may be:
//   - a string (type name only): "cep": "string"
//   - an object with "type", optional "description", and optional "enum":
//     "unit": {"type": "string", "description": "Temperature unit", "enum": ["celsius", "fahrenheit"]}
//
// Type names are whitelisted; description and enum are passed through so providers can show them to the LLM.
func argsToJSONSchema(args map[string]any) (json.RawMessage, error) {
	if len(args) == 0 {
		return niro.JSONMarshal(map[string]any{"type": "object", "properties": map[string]any{}})
	}
	props := make(map[string]any)
	required := make([]string, 0, len(args))
	for name, v := range args {
		prop, err := argToSchemaProperty(v)
		if err != nil {
			return nil, niro.WrapErrorf(niro.ErrCodeInvalidRequest, "dsl: arg %q", err, name)
		}
		props[name] = prop
		required = append(required, name)
	}
	out := map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
	return niro.JSONMarshal(out)
}

// argToSchemaProperty builds a JSON Schema property from a DSL arg value (string or object).
func argToSchemaProperty(v any) (map[string]any, error) {
	switch val := v.(type) {
	case string:
		return map[string]any{"type": jsonSchemaType(val)}, nil
	case map[string]any:
		typeName := ""
		if t, ok := val["type"].(string); ok {
			typeName = jsonSchemaType(t)
		}
		if typeName == "" {
			typeName = "string"
		}
		prop := map[string]any{"type": typeName}
		if desc, ok := val["description"].(string); ok && desc != "" {
			prop["description"] = desc
		}
		if enumVal, ok := val["enum"]; ok {
			enumSlice, err := enumToSlice(enumVal)
			if err != nil {
				return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "enum", err)
			}
			if len(enumSlice) > 0 {
				prop["enum"] = enumSlice
			}
		}
		return prop, nil
	default:
		return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest, "arg must be type string or object with type (got %T)", v)
	}
}

func jsonSchemaType(s string) string {
	switch s {
	case "number", "integer", "boolean", "array", "object":
		return s
	default:
		return "string"
	}
}

// schemaForSingleEnumProp returns JSON Schema for an object with one required property (type string, optional description, enum).
func schemaForSingleEnumProp(name, description string, enum []any) (json.RawMessage, error) {
	prop := map[string]any{"type": "string"}
	if description != "" {
		prop["description"] = description
	}
	if len(enum) > 0 {
		prop["enum"] = enum
	}
	return niro.JSONMarshal(map[string]any{
		"type":       "object",
		"properties": map[string]any{name: prop},
		"required":   []string{name},
	})
}

// enumToSlice validates and normalizes an enum value from the DSL (JSON array of primitives).
func enumToSlice(v any) ([]any, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest, "must be an array (got %T)", v)
	}
	out := make([]any, 0, len(arr))
	for i, item := range arr {
		switch item.(type) {
		case string, float64, bool, nil:
			out = append(out, item)
		default:
			return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest, "enum[%d]: only string, number, boolean allowed (got %T)", i, item)
		}
	}
	return out, nil
}
