package dsl

import (
	"context"
	"encoding/json"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/tools"
)

func init() {
	RegisterToolType("handoff", ToolTypeBuilderFunc(BuildHandoffTool))
}

// HandoffSpec is the DSL shape for type "handoff" tools.
// Handoff is a tool like any other in the DSL: define it in agents.json with type "handoff",
// description, and args/allow; the model calls it; the handler returns tools.HandoffSignal so
// the tool loop emits a handoff frame. The runner executes the target via RunHandoff.
// Description is a template string (expanded at request time with RunContext).
// Either provide args.target with type, description, enum, or use allow as shorthand for target enum.
type HandoffSpec struct {
	Type        string                 `json:"type"`
	Description string                 `json:"description"`
	Allow       []string               `json:"allow,omitempty"` // shorthand: target enum when args not used
	Args        map[string]any `json:"args,omitempty"`  // full args; if present, used for schema
}

// BuildHandoffTool builds a tools.ToolDefinition for a type "handoff" tool.
// The handler returns tools.HandoffSignal so the tool loop emits a handoff frame.
func BuildHandoffTool(name string, spec json.RawMessage) (tools.ToolDefinition, error) {
	s, err := niro.UnmarshalTo[HandoffSpec](spec)
	if err != nil {
		return tools.ToolDefinition{}, niro.WrapError(niro.ErrCodeInvalidRequest, "handoff tool spec", err)
	}
	if s.Description == "" {
		return tools.ToolDefinition{}, niro.NewErrorf(niro.ErrCodeInvalidRequest, "handoff tool %q: description is required", name)
	}
	var schema json.RawMessage
	if len(s.Args) > 0 {
		schema, err = argsToJSONSchema(s.Args)
		if err != nil {
			return tools.ToolDefinition{}, err
		}
	} else if len(s.Allow) > 0 {
		enum := make([]any, len(s.Allow))
		for i, a := range s.Allow {
			enum[i] = a
		}
		schema, err = schemaForSingleEnumProp("target", "Agent or workflow to route the conversation to", enum)
		if err != nil {
			return tools.ToolDefinition{}, err
		}
	} else {
		return tools.ToolDefinition{}, niro.NewErrorf(niro.ErrCodeInvalidRequest, "handoff tool %q: args or allow is required", name)
	}
	handler := func(ctx context.Context, args json.RawMessage) (any, error) {
		m, err := niro.UnmarshalTo[map[string]any](args)
		if err != nil {
			return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "handoff args", err)
		}
		var t string
		if m != nil {
			t, _ = m["target"].(string)
		}
		return nil, tools.HandoffSignal{Target: t}
	}
	return tools.NewToolDefinition(name, s.Description, schema, handler)
}
