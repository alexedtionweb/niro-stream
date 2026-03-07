// Tool type registry: register primitives (e.g. "http") or custom types.
// In the DSL, each tool spec can include "type": "http" (or any registered type name).
// Omit "type" or use "code" for tools implemented via CompileOptions.ToolHandlers.
// Built-in "http": URL (Go template with args), method, headers, args → JSON Schema.
// Add types with RegisterToolType or CompileOptions.ToolTypes (option overrides default).
package dsl

import (
	"encoding/json"
	"sync"

	"github.com/alexedtionweb/niro-stream/tools"
)

// ToolTypeBuilder builds a tools.ToolDefinition from a tool's raw DSL spec.
// Register with RegisterToolType or pass via CompileOptions.ToolTypes so that
// tools with "type": "http" (or custom type names) are built by the corresponding builder.
type ToolTypeBuilder interface {
	// Build returns a ToolDefinition for the given tool name and spec.
	// The spec is the raw JSON value for this tool from the DSL (e.g. {"type":"http","url":"...","args":{...}}).
	Build(name string, spec json.RawMessage) (tools.ToolDefinition, error)
}

// ToolTypeBuilderFunc adapts a function to ToolTypeBuilder.
type ToolTypeBuilderFunc func(name string, spec json.RawMessage) (tools.ToolDefinition, error)

func (f ToolTypeBuilderFunc) Build(name string, spec json.RawMessage) (tools.ToolDefinition, error) {
	return f(name, spec)
}

var (
	defaultToolTypes   = make(map[string]ToolTypeBuilder)
	defaultToolTypesMu sync.RWMutex
)

// RegisterToolType registers a tool type globally (e.g. "http").
// Typically called from init() or at startup. Compile uses both registered types
// and CompileOptions.ToolTypes (option overrides take precedence).
func RegisterToolType(typeName string, b ToolTypeBuilder) {
	if typeName == "" || b == nil {
		return
	}
	defaultToolTypesMu.Lock()
	defer defaultToolTypesMu.Unlock()
	defaultToolTypes[typeName] = b
}

func getToolType(typeName string, opts CompileOptions) ToolTypeBuilder {
	if opts.ToolTypes != nil {
		if b := opts.ToolTypes[typeName]; b != nil {
			return b
		}
	}
	defaultToolTypesMu.RLock()
	b := defaultToolTypes[typeName]
	defaultToolTypesMu.RUnlock()
	return b
}
