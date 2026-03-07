package dsl

import (
	"encoding/json"

	"github.com/alexedtionweb/niro-stream"
)

// Validate checks that the agent definition is semantically valid:
// structure, references (tools, schemas, handoff agents), and optional expr syntax.
// Call after ParseDSL and before Compile.
func Validate(def *DSLDefinition) error {
	if def == nil {
		return niro.NewError(niro.ErrCodeInvalidRequest, "dsl: validate: definition is nil")
	}

	// Optional: empty top-level is allowed; agents may reference no tools
	if def.Agents == nil {
		def.Agents = make(map[string]AgentConfig)
	}
	if def.Tools == nil {
		def.Tools = make(map[string]json.RawMessage)
	}
	if def.Schemas == nil {
		def.Schemas = make(map[string]json.RawMessage)
	}

	for name, cfg := range def.Agents {
		if err := validateAgent(name, cfg, def); err != nil {
			return err
		}
	}
	return nil
}

func validateAgent(name string, cfg AgentConfig, def *DSLDefinition) error {
	if cfg.Model == "" {
		return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: agent %q: model is required", name)
	}
	if cfg.Prompt.Template == "" {
		return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: agent %q: prompt.template is required", name)
	}

	for _, ref := range cfg.Tools {
		if ref.Name == "" {
			return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: agent %q: tool ref has empty name", name)
		}
		if _, ok := def.Tools[ref.Name]; !ok {
			return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: agent %q: tool %q not found in tools", name, ref.Name)
		}
		if ref.When != "" {
			if err := CompileRunContextExpr(ref.When); err != nil {
				return niro.WrapErrorf(niro.ErrCodeInvalidRequest, "dsl: agent %q: tool %q when", err, name, ref.Name)
			}
		}
		if ref.Unless != "" {
			if err := CompileRunContextExpr(ref.Unless); err != nil {
				return niro.WrapErrorf(niro.ErrCodeInvalidRequest, "dsl: agent %q: tool %q unless", err, name, ref.Name)
			}
		}
	}

	for _, schemaName := range cfg.Output.Schemas { // modality -> schema name
		if _, ok := def.Schemas[schemaName]; !ok {
			return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: agent %q: output.schemas references %q not found in schemas", name, schemaName)
		}
	}

	if cfg.Limits.MaxToolCalls < 0 {
		return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: agent %q: limits.max_tool_calls must be >= 0", name)
	}
	if cfg.Limits.MaxParallelTools < 0 {
		return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: agent %q: limits.max_parallel_tools must be >= 0", name)
	}

	return nil
}
