package dsl

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/tools"
)

const sampleAgentJSON = `{
  "tools": {
    "handoff": {
      "type": "handoff",
      "description": "Route to another agent.",
      "args": { "target": { "type": "string", "description": "Agent to route to", "enum": ["billing_agent"] } }
    },
    "lookup_order": { "args": { "order_id": "string" } },
    "cancel_order": { "args": { "order_id": "string" } }
  },
  "schemas": {
    "voice_response": { "type": "object", "properties": { "text": { "type": "string" } } }
  },
  "agents": {
    "support_agent": {
      "model": "gpt-4o-mini",
      "model_config": { "temperature": 0.2, "max_tokens": 300 },
      "prompt": { "template": "You are support. User: {{.event.text}}\nAssistant:" },
      "tools": [
        { "name": "handoff" },
        { "name": "lookup_order" },
        { "name": "cancel_order", "when": "session.order_loaded", "unless": "session.order.status == \"shipped\"" }
      ],
      "tool_choice": "auto",
      "output": { "stream": true },
      "limits": { "max_tool_calls": 4 }
    },
    "billing_agent": {
      "model": "gpt-4o-mini",
      "model_config": { "max_tokens": 200 },
      "prompt": { "template": "You are billing. User: {{.event.text}}\nAssistant:" },
      "tools": [],
      "tool_choice": "none"
    }
  }
}`

func TestParseDSL(t *testing.T) {
	def, err := ParseDSL([]byte(sampleAgentJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if def == nil {
		t.Fatal("def is nil")
	}
	if len(def.Tools) != 3 {
		t.Errorf("want 3 tools, got %d", len(def.Tools))
	}
	if len(def.Agents) != 2 {
		t.Errorf("want 2 agents, got %d", len(def.Agents))
	}
	if def.Agents["support_agent"].Model != "gpt-4o-mini" {
		t.Errorf("support_agent model: got %q", def.Agents["support_agent"].Model)
	}
}

func TestValidate(t *testing.T) {
	def, _ := ParseDSL([]byte(sampleAgentJSON))
	err := Validate(def)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidateMissingToolRef(t *testing.T) {
	bad := `{"tools":{},"schemas":{},"agents":{"x":{"model":"gpt-4","prompt":{"template":"Hi"},"tools":[{"name":"nonexistent"}]}}}`
	def, _ := ParseDSL([]byte(bad))
	err := Validate(def)
	if err == nil {
		t.Fatal("expected error for missing tool ref")
	}
}

func TestCompile(t *testing.T) {
	def, _ := ParseDSL([]byte(sampleAgentJSON))
	_ = Validate(def)
	handlers := map[string]tools.ToolHandler{
		"lookup_order": func(ctx context.Context, args json.RawMessage) (any, error) { return "ok", nil },
		"cancel_order": func(ctx context.Context, args json.RawMessage) (any, error) { return "ok", nil },
	}
	nd, err := def.Compile(CompileOptions{ToolHandlers: handlers})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if nd.Toolset.Len() != 3 {
		t.Errorf("want 3 tools in toolset, got %d", nd.Toolset.Len())
	}
	if nd.Agents["support_agent"] == nil {
		t.Fatal("support_agent not in compiled agents")
	}
	if nd.Agents["support_agent"].ToolChoice != niro.ToolChoiceAuto {
		t.Errorf("tool_choice: got %v", nd.Agents["support_agent"].ToolChoice)
	}
}

func TestRunContext(t *testing.T) {
	rc := NewRunContext()
	rc.Set("session", map[string]any{"order_loaded": true})
	rc.Set("event", map[string]any{"text": "hello"})
	v, ok := rc.Get("event")
	if !ok {
		t.Fatal("event not found")
	}
	m, _ := v.(map[string]any)
	if m["text"] != "hello" {
		t.Errorf("event.text: got %v", m["text"])
	}
	snap := rc.Snapshot()
	if len(snap) != 2 {
		t.Errorf("snapshot len: got %d", len(snap))
	}
}

func TestEvalCondition(t *testing.T) {
	rc := NewRunContext()
	rc.Set("session", map[string]any{"order_loaded": true, "order": map[string]any{"status": "shipped"}})
	ok, err := EvalCondition("session.order_loaded", rc)
	if err != nil {
		t.Fatalf("when: %v", err)
	}
	if !ok {
		t.Error("expected session.order_loaded true")
	}
	ok, err = EvalCondition("session.order.status == \"shipped\"", rc)
	if err != nil {
		t.Fatalf("unless: %v", err)
	}
	if !ok {
		t.Error("expected session.order.status == \"shipped\" true")
	}
	// event.text (string) is truthy when non-empty, falsy when empty
	rc.Set("event", map[string]any{"text": "onde fica o cep 02309100"})
	ok, err = EvalCondition("event.text", rc)
	if err != nil {
		t.Fatalf("event.text: %v", err)
	}
	if !ok {
		t.Error("expected event.text (non-empty string) to be truthy")
	}
	rc.Set("event", map[string]any{"text": ""})
	ok, err = EvalCondition("event.text", rc)
	if err != nil {
		t.Fatalf("event.text empty: %v", err)
	}
	if ok {
		t.Error("expected event.text (empty string) to be falsy")
	}
	// Reserved defaults: session/event/history are never nil, so expressions like session.disable_cep == true do not error
	rc = NewRunContext()
	rc.Set("event", map[string]any{"text": "hello"})
	ok, err = EvalCondition("session.disable_cep == true", rc)
	if err != nil {
		t.Fatalf("session.disable_cep with default session: %v", err)
	}
	if ok {
		t.Error("expected session.disable_cep == true to be false when session has no disable_cep")
	}
}

func TestExpandPrompt(t *testing.T) {
	rc := NewRunContext()
	rc.Set("event", map[string]any{"text": "Where is my order?"})
	out, err := expandPrompt("User: {{.event.text}}\nAssistant:", rc)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if out != "User: Where is my order?\nAssistant:" {
		t.Errorf("got %q", out)
	}
}

func TestParseWorkflow(t *testing.T) {
	wfJSON := `{"name":"w1","type":"fan","agents":["support_agent","billing_agent"]}`
	wf, err := ParseWorkflow([]byte(wfJSON))
	if err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	if wf.Type != "fan" || len(wf.Agents) != 2 {
		t.Errorf("type=%q agents=%d", wf.Type, len(wf.Agents))
	}
}

func TestValidateWorkflow(t *testing.T) {
	wf, _ := ParseWorkflow([]byte(`{"name":"w1","type":"fan","agents":["support_agent","billing_agent"]}`))
	agentNames := map[string]struct{}{"support_agent": {}, "billing_agent": {}}
	err := ValidateWorkflow(wf, agentNames)
	if err != nil {
		t.Fatalf("validate workflow: %v", err)
	}
}

func TestValidateWorkflowMissingAgent(t *testing.T) {
	wf, _ := ParseWorkflow([]byte(`{"name":"w1","type":"fan","agents":["support_agent","unknown"]}`))
	agentNames := map[string]struct{}{"support_agent": {}}
	err := ValidateWorkflow(wf, agentNames)
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
}

func TestCompileWorkflow(t *testing.T) {
	wf, _ := ParseWorkflow([]byte(`{"workflows":{"p":{"type":"sequence","agents":["support_agent","billing_agent"]}}}`))
	agentNames := map[string]struct{}{"support_agent": {}, "billing_agent": {}}
	compiled, err := wf.CompileWorkflow(agentNames)
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	if compiled["p"] == nil || compiled["p"].Type != "sequence" {
		t.Errorf("compiled workflow p: %+v", compiled["p"])
	}
}

func TestCompileHTTPToolType(t *testing.T) {
	def, _ := ParseDSL([]byte(`{
	  "tools": {
	    "get_order": {
	      "type": "http",
	      "description": "Get order by id",
	      "input": { "type": "object", "required": ["order_id"], "properties": { "order_id": { "type": "string" } } },
	      "method": "GET",
	      "url": "https://api.example.com/orders/{{.order_id}}"
	    }
	  },
	  "schemas": {},
	  "agents": {}
	}`))
	def.Agents = make(map[string]AgentConfig)
	nd, err := def.Compile(CompileOptions{})
	if err != nil {
		t.Fatalf("compile http tool: %v", err)
	}
	if nd.Toolset.Len() != 1 {
		t.Fatalf("want 1 tool, got %d", nd.Toolset.Len())
	}
	td, ok := nd.Toolset.Get("get_order")
	if !ok {
		t.Fatal("get_order not in toolset")
	}
	if td.Name != "get_order" || td.Handler == nil {
		t.Errorf("tool def: name=%q handler=%v", td.Name, td.Handler == nil)
	}
}

func TestCompileArgsWithEnum(t *testing.T) {
	def, _ := ParseDSL([]byte(`{
	  "tools": {
	    "get_weather": {
	      "type": "http",
	      "description": "Get weather",
	      "input": { "type": "object", "required": ["unit"], "properties": { "unit": { "type": "string", "description": "Temperature unit", "enum": ["celsius", "fahrenheit"] } } },
	      "method": "GET",
	      "url": "https://api.example.com/weather"
	    }
	  },
	  "schemas": {},
	  "agents": {}
	}`))
	def.Agents = make(map[string]AgentConfig)
	nd, err := def.Compile(CompileOptions{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	td, ok := nd.Toolset.Get("get_weather")
	if !ok {
		t.Fatal("get_weather not in toolset")
	}
	var schema map[string]any
	if err := niro.JSONUnmarshal(td.Schema, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		t.Fatal("schema has no properties")
	}
	unit, _ := props["unit"].(map[string]any)
	if unit == nil {
		t.Fatal("property unit missing")
	}
	enumVal, ok := unit["enum"].([]any)
	if !ok || len(enumVal) != 2 {
		t.Errorf("unit.enum want 2 items, got %v", unit["enum"])
	}
	if unit["description"] != "Temperature unit" {
		t.Errorf("unit.description = %v", unit["description"])
	}
}

func TestRegisterToolType(t *testing.T) {
	customCalled := false
	RegisterToolType("custom", ToolTypeBuilderFunc(func(name string, spec json.RawMessage) (tools.ToolDefinition, error) {
		customCalled = true
		return tools.NewToolDefinition(name, "custom", json.RawMessage(`{"type":"object"}`),
			func(context.Context, json.RawMessage) (any, error) { return "ok", nil })
	}))
	def, _ := ParseDSL([]byte(`{"tools":{"x":{"type":"custom"}},"schemas":{},"agents":{}}`))
	def.Agents = make(map[string]AgentConfig)
	nd, err := def.Compile(CompileOptions{})
	if err != nil {
		t.Fatalf("compile custom: %v", err)
	}
	if !customCalled {
		t.Error("custom builder was not called")
	}
	if nd.Toolset.Len() != 1 {
		t.Errorf("want 1 tool, got %d", nd.Toolset.Len())
	}
}
