# DSL and workflow JSON reference

This document describes every JSON field used to define **agents** (plugin/dsl and plugin/agent) and **workflows** (plugin/dsl). Use it when authoring or validating definition files.

---

## 1. Agent definition (DSL) — `plugin/dsl`

Parsed with `ParseDSL` / `ParseDSLFile`; validated with `Validate`; compiled with `Compile` to get a `NiroDefinition` and then a `Runner`.

### Top-level keys

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| **tools** | object | No (default: `{}`) | Map of tool name → tool spec. Each spec can have `"type"` (e.g. `"http"`, `"handoff"`, `"code"`) and type-specific fields. Omit `type` or use `"code"` for tools implemented via `CompileOptions.ToolHandlers`. |
| **schemas** | object | No (default: `{}`) | Map of schema name → JSON Schema (raw). Referenced by agents in `output.schemas`. |
| **agents** | object | No (default: `{}`) | Map of agent name → agent config. See [Agent config](#agent-config) below. |

### Agent config

Each entry in `agents` is an object with:

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| **model** | string | **Yes** | Model ID (e.g. `gpt-4o`, `claude-3-5-sonnet`). Passed to `Request.Model`. |
| **model_config** | object | No | Overrides for temperature, max_tokens, thinking_budget. See [Model config](#model-config). |
| **prompt** | object | **Yes** | System prompt. See [Prompt config](#prompt-config). |
| **tools** | array | No | List of tool references (name + optional when/unless). See [Agent tool ref](#agent-tool-ref). |
| **tool_choice** | string | No | `"auto"` (default), `"none"`, `"required"`, or `"func:<name>"` for forced tool. |
| **output** | object | No | Allowed output types, modalities, schemas, stream. See [Output config](#output-config). |
| **limits** | object | No | Max tool calls and max parallel tools. See [Limits config](#limits-config). |

#### Model config

| Key | Type | Description |
|-----|------|-------------|
| **temperature** | number (0–2) or null | Sampling temperature. Omit or null = provider default. |
| **max_tokens** | integer | Max output tokens per turn. |
| **thinking_budget** | integer or null | Max reasoning/thinking tokens (e.g. Gemini 2.5). Omit or null = provider default; 0 = disable. |

#### Prompt config

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| **template** | string | **Yes** | System prompt. Go `text/template` with **RunContext** as data (e.g. `{{.session.user_id}}`, `{{.event.text}}`, `{{.history}}`). |

#### Agent tool ref

Each element in `agents.<name>.tools`:

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| **name** | string | **Yes** | Tool name; must exist in top-level `tools`. |
| **when** | string | No | Expr expression. If set, tool is included only when expression evaluates truthy. Env: RunContext (session, event, history). |
| **unless** | string | No | Expr expression. If set, tool is excluded when expression evaluates truthy. |

**Expr**: [expr-lang/expr](https://github.com/expr-lang/expr) syntax. Reserved RunContext keys: `session`, `event`, `history`. Example: `session.role == "admin"`, `event.text != ""`.

#### Output config

| Key | Type | Description |
|-----|------|-------------|
| **allow** | array of strings | Allowed output kinds (e.g. text, tool_calls). |
| **modalities** | array of strings | Allowed modalities. |
| **schemas** | object | Map of modality/slot → schema name (must exist in top-level `schemas`). |
| **stream** | boolean | Whether to stream output. |

#### Limits config

| Key | Type | Description |
|-----|------|-------------|
| **max_tool_calls** | integer | Max tool calls per turn. Must be ≥ 0. |
| **max_parallel_tools** | integer | Max parallel tool invocations. Must be ≥ 0. |

---

### Tool specs (top-level `tools`)

Each tool is a JSON object. The **type** field determines which builder is used.

#### Common (all types)

| Key | Type | Description |
|-----|------|-------------|
| **type** | string | `"http"`, `"handoff"`, `"code"`, or custom registered type. Omit or `"code"` = use `CompileOptions.ToolHandlers[name]`. |
| **description** | string | Shown to the model. For handoff, supports Go text/template with RunContext. |

#### Type: `code` (or omit type)

| Key | Type | Description |
|-----|------|-------------|
| **args** | object | Parameter schema. Each key = arg name. Value = type string (e.g. `"string"`, `"number"`, `"integer"`, `"boolean"`, `"array"`, `"object"`) or object: `{"type":"string","description":"...","enum":["a","b"]}`. All args are required. |

Handler is supplied at compile time via `CompileOptions.ToolHandlers[name]`.

#### Type: `http`

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| **type** | string | Yes | `"http"`. |
| **description** | string | No | Tool description. |
| **input** | object | Yes | JSON Schema for the tool’s parameters (e.g. `{"type":"object","required":["order_id"],"properties":{"order_id":{"type":"string"}}}`). |
| **method** | string | Yes | HTTP method (e.g. `GET`, `POST`). |
| **url** | string | Yes | URL. Go text/template with RunContext + call args as data. |
| **headers** | object | No | Map of header name → value. Values are templates (RunContext + args). |
| **timeout** | string | No | Go duration (e.g. `30s`). Default 30s. |
| **output** | string | No | `"text"` = return raw body as string; `"json"` or omit = use **cases** or decode JSON. |
| **cases** | array | No | Response handling. Each element: **when** (expr string, env `$` = `{status, body}`), **parse** (map for extraction). First matching when wins. |
| **retry** | object | No | **max_retries** (int), **backoff** (duration string). |

#### Type: `handoff`

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| **type** | string | Yes | `"handoff"`. |
| **description** | string | **Yes** | Shown to the model; template with RunContext. |
| **allow** | array of strings | No* | Shorthand: list of allowed target names (agent or workflow). Becomes enum for a single `target` parameter. *Required if **args** is omitted. |
| **args** | object | No* | Full args schema (e.g. `{"target":{"type":"string","description":"Agent to route to","enum":["billing_agent"]}}`). *Required if **allow** is omitted. |

Handler returns `tools.HandoffSignal{Target: "<name>"}`; runner uses `RunHandoff` to execute the target (workflow or agent).

---

## 2. Workflow definition (DSL) — `plugin/dsl`

Parsed with `ParseWorkflow` / `ParseWorkflowFile`; validated with `ValidateWorkflow(definition, agentNames)`; compiled with `CompileWorkflow(agentNames)` to get a map of `CompiledWorkflow`. All referenced agent names must exist in the compiled agent definition.

### Single-workflow form

One workflow at the top level:

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| **name** | string | No | Workflow name (used as key when compiling). |
| **description** | string | No | Human-readable description. |
| **type** | string | **Yes** | `"fan"` \| `"race"` \| `"sequence"`. |
| **agents** | array of strings | **Yes** | List of agent names. Order matters for `sequence`; for `fan`/`race` it is the set of parallel agents. |

**Note:** Single-workflow form does **not** support `fan_then`. Use the multi-workflow form for `fan_then`.

### Multi-workflow form

Multiple named workflows under a `workflows` map:

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| **workflows** | object | **Yes** | Map of workflow name → workflow node. See [Workflow node](#workflow-node). |

Top-level **name**, **description**, **type**, **agents** are ignored when **workflows** is present.

#### Workflow node

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| **description** | string | No | Human-readable description. |
| **type** | string | **Yes** | `"fan"` \| `"race"` \| `"sequence"` \| `"fan_then"`. |
| **agent** | string | No* | Single agent name (entry point). Use for one-agent workflows (e.g. chat). *One of **agent** or **agents** required. |
| **agents** | array of strings | No* | Agent names. For `fan`/`race`/`sequence`: list of agents. For `fan_then`: parallel agents; their outputs are passed to **then_agent**. *One of **agent** or **agents** required. |
| **then_agent** | string | Yes for `fan_then` | Agent that receives the combined result of the parallel **agents** (e.g. debate → synthesizer). Must be a valid agent name. |

**Workflow types:**

- **fan** — Run agents in parallel; merge streams (e.g. multi-perspective).
- **race** — Run agents in parallel; first non-empty response wins (e.g. hedging).
- **sequence** — Run agents in order; output of one is input to the next.
- **fan_then** — Run **agents** in parallel, then run **then_agent** with those results (e.g. debate then summarize).

---

## 3. Agent definition (plugin/agent) — Orchestrator steps

Used by **plugin/agent** for the programmatic **Orchestrator** and loadable via `LoadAgentDefinition(path)`. This is a **separate** format from the DSL agent definition above (different JSON shape and purpose).

### Top-level: AgentDefinition

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| **name** | string | No | Agent name (metadata). |
| **description** | string | No | Human-readable description. |
| **steps** | array | **Yes** | Ordered list of steps. See [Step](#step). |

### Step

Each step object:

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| **type** | string | **Yes** | `"llm"` \| `"tool"` \| `"peer"`. |
| **input** | string | No | Input for this step. If empty, the **previous step’s output** is used. |
| **output_var** | string | No | Optional name for this step’s output (metadata; not used for chaining). |
| **tool_name** | string | For `type: "tool"` | Tool name (must exist in the Orchestrator’s Toolset). |
| **tool_args** | object/array | For `type: "tool"` | JSON payload for the tool call. |
| **peer_name** | string | For `type: "peer"` | Name of the registered Peer (agent) to call. |

**Execution:**

- **llm** — `Orchestrator.Runtime.Run(ctx, sessionID, input)`; output = response text.
- **tool** — `Toolset.ExecuteCall(ctx, ToolCall{Name: tool_name, Args: tool_args})`; output = result content.
- **peer** — `Runtime.CallPeer(ctx, peer_name, sessionID, input)`; output = peer response text.

**Example (agent definition JSON for plugin/agent):**

```json
{
  "name": "helper",
  "description": "LLM then tool then LLM",
  "steps": [
    { "type": "llm", "input": "Summarize the user request in one line." },
    { "type": "tool", "tool_name": "fetch_data", "tool_args": { "id": "123" } },
    { "type": "llm" }
  ]
}
```

---

## 4. Quick reference

| Definition | File / API | Parse | Validate | Compile / Run |
|------------|------------|-------|----------|----------------|
| DSL (agents + tools) | JSON | `ParseDSL` / `ParseDSLFile` | `Validate` | `Compile(opts)` → `NiroDefinition` → `NewRunner(nd, provider)` |
| Workflows | JSON | `ParseWorkflow` / `ParseWorkflowFile` | `ValidateWorkflow(wf, agentNames)` | `CompileWorkflow(agentNames)` → `BindRunner` / `BindWorkflows` |
| Agent steps (Orchestrator) | JSON or Go | `LoadAgentDefinition(path)` or struct | — | `Orchestrator.RunDefinition(ctx, sessionID, def)` |

**RunContext** (session, event, history and custom keys) is used for:

- Prompt **template** expansion (Go `text/template`).
- Tool **when** / **unless** (expr).
- HTTP tool **url** / **headers** (templates with RunContext + call args).

See [INTEGRATION.md](INTEGRATION.md) for wiring the DSL and agent plugin into your project, and [plugin/dsl](https://pkg.go.dev/github.com/alexedtionweb/niro-stream/plugin/dsl) / [plugin/agent](https://pkg.go.dev/github.com/alexedtionweb/niro-stream/plugin/agent) for API details.
