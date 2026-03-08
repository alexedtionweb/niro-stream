# Integrating Niro into Your Project

This guide explains how to add Niro to an existing Go application for streaming LLM calls, tools, and observability.

---

## 1. Dependencies and Modules

Niro uses a **multi-module** layout. Depend only on what you use.

### Core (required)

```bash
go get github.com/alexedtionweb/niro-stream
```

Core has **zero external dependencies**. It defines `Provider`, `Request`, `Stream`, `Frame`, `Options`, errors, and validation.

### Provider (choose one or more)

| Provider   | Install |
|-----------|---------|
| OpenAI    | `go get github.com/alexedtionweb/niro-stream/provider/openai` |
| Anthropic | `go get github.com/alexedtionweb/niro-stream/provider/anthropic` |
| Google Gemini | `go get github.com/alexedtionweb/niro-stream/provider/google` |
| AWS Bedrock | `go get github.com/alexedtionweb/niro-stream/provider/bedrock` |
| OpenAI-compatible (Ollama, vLLM) | In core: `provider/compat` — no extra install |

### Optional packages (same repo)

- **Runtime + hooks + pipeline**: `runtime`, `hook`, `pipe`
- **Tool loop and toolset**: `tools`
- **Orchestration**: `orchestrate` (Fan, Race, Sequence)
- **Output routing**: `output` (sinks)
- **Middleware**: `middleware` (retry, timeout, cache, tracing)
- **Registry / multi-tenancy**: `registry`
- **Structured output**: `structured`
- **Agent plugin**: `plugin/agent`
- **DSL plugin**: `plugin/dsl`

---

## 2. Minimal Integration

```go
package main

import (
    "context"
    "fmt"
    "os"
    "github.com/alexedtionweb/niro-stream"
    "github.com/alexedtionweb/niro-stream/provider/openai"
)

func main() {
    ctx := context.Background()
    p := openai.New(os.Getenv("OPENAI_API_KEY"))

    stream, err := p.Generate(ctx, &niro.Request{
        Model:        "gpt-4o",
        SystemPrompt: "Be concise.",
        Messages:     []niro.Message{niro.UserText("What is 2+2?")},
        Options:      niro.Options{MaxTokens: 256, Temperature: niro.Temp(0.7)},
    })
    if err != nil {
        fmt.Fprintf(os.Stderr, "error: %v\n", err)
        os.Exit(1)
    }

    for stream.Next(ctx) {
        fmt.Print(stream.Frame().Text)
    }
    if err := stream.Err(); err != nil {
        fmt.Fprintf(os.Stderr, "stream error: %v\n", err)
        os.Exit(1)
    }
    fmt.Println()
    u := stream.Usage()
    fmt.Printf("tokens: in=%d out=%d\n", u.InputTokens, u.OutputTokens)
}
```

---

## 3. Validation and Errors

Validate before calling:

```go
if err := req.Validate(); err != nil {
    return err // *niro.Error with Code, Message, Retryable
}
stream, err := p.Generate(ctx, req)
```

Handle by category:

```go
if err != nil {
    if niro.IsRetryable(err) { /* retry */ }
    if niro.IsRateLimited(err) { /* backoff */ }
    if niro.IsAuthError(err) { /* do not retry */ }
    if niro.IsTimeout(err) { /* retry or fail */ }
}
```

---

## 4. Hooks (Observability)

```go
import "github.com/alexedtionweb/niro-stream/runtime"
import "github.com/alexedtionweb/niro-stream/hook"

type LogHook struct{ hook.NoOpHook }
func (LogHook) OnGenerateStart(ctx context.Context, info hook.GenerateStartInfo) context.Context {
    log.Printf("start: model=%s", info.Model)
    return ctx
}
func (LogHook) OnGenerateEnd(ctx context.Context, info hook.GenerateEndInfo) {
    log.Printf("end: usage=%+v duration=%v", info.Usage, info.Duration)
}

rt := runtime.New(p).WithHook(LogHook{})
stream, err := rt.Generate(ctx, req)
```

Use `hook.Compose(h1, h2)` for multiple hooks.

---

## 5. Output Routing (Sinks)

Tee stream to UI/logs without changing content:

```go
import "github.com/alexedtionweb/niro-stream/output"

sink := &output.Sink{
    OnText: func(ctx context.Context, text string) error { fmt.Print(text); return nil },
    OnEnd:  func(ctx context.Context, u niro.Usage, resp *niro.ResponseMeta) error {
        log.Printf("end: in=%d out=%d finish_reason=%s", u.InputTokens, u.OutputTokens, resp.FinishReason)
        return nil
    },
}
routed := output.Route(ctx, stream, sink)
// Consume routed as usual
```

For multiple agents use `output.RouteAgent(ctx, stream, sink, agentName)` and `output.AgentFromContext(ctx)` in callbacks.

---

## 6. Tool Calling

Automatic loop:

```go
import "github.com/alexedtionweb/niro-stream/tools"

exec := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
    switch name {
    case "get_weather": return getWeather(ctx, args)
    default: return "", fmt.Errorf("unknown tool %q", name)
    }
})
loop := tools.NewToolLoop(exec, 5)
stream, err := loop.GenerateWithTools(ctx, p, req)
```

For schema validation and hooks use `tools.NewToolset()` and `tools.NewToolingProvider(p, toolset, opts)`.

---

## 7. Middleware

```go
import "github.com/alexedtionweb/niro-stream/middleware"

retried := middleware.NewRetryProvider(p, middleware.RetryConfig{
    MaxAttempts: 5,
    Backoff:     middleware.ExponentialBackoff{InitialDelay: 100 * time.Millisecond, Multiplier: 2, Jitter: true},
    ShouldRetry: niro.IsRetryable,
})
timed := middleware.NewTimeoutProvider(retried, 5*time.Minute)
// Use timed as your Provider
```

---

## 8. Registry and Multi-Tenancy

```go
import "github.com/alexedtionweb/niro-stream/registry"

reg := registry.New()
reg.Register("openai", openaiProvider)
reg.Register("anthropic", anthropicProvider)
stream, err := reg.Generate(ctx, "openai", req)

router := registry.NewMultiTenantProvider(reg, registry.WithDefaultClient("openai"))
stream, err := router.Generate(ctx, &niro.Request{Client: "anthropic", Messages: msgs})
```

---

## 9. Orchestration

```go
import "github.com/alexedtionweb/niro-stream/orchestrate"

stream := orchestrate.Fan(ctx, gen1, gen2, gen3)                    // parallel merge
text, usage, err := orchestrate.Race(ctx, gen1, gen2)               // first wins
stream, err := orchestrate.Sequence(ctx, step1, step2)              // chained
```

---

## 10. Structured Output

```go
import "github.com/alexedtionweb/niro-stream/structured"

type Result struct { Answer int `json:"answer"` }
schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}`)
result, resp, usage, err := structured.GenerateStructured[Result](ctx, p, &niro.Request{
    Messages: []niro.Message{niro.UserText("What is 2+2? JSON.")},
    ResponseFormat: "json_schema", ResponseSchema: schema,
})
```

---

## 11. Agent plugin

The **agent plugin** (`plugin/agent`) adds a conversational layer on top of a Niro provider: **memory** (per-session history), **system prompt**, **components** (e.g. tools), and **peers** (other agents). It is optional; use it when you need stateful, multi-turn conversations with a single agent identity.

### What the Agent is

- **Agent** wraps a `Provider` and adds:
  - **Memory**: load/save conversation history by `sessionID` (you implement the backend: SQL, Redis, in-memory, etc.).
  - **History policy**: how much history is sent each turn (e.g. sliding window, no history).
  - **System prompt**: fixed instructions for every turn.
  - **Components**: plugins that run at startup and can inject tools or behavior (e.g. `ToolingComponent`).
  - **Peers**: named agents you can call from this agent via `CallPeer(ctx, peerName, sessionID, input)`.

- Each **turn** works as: load history for `sessionID` → trim with history policy → append new user message → call provider → stream or collect response → append assistant message to history → save.

### Basic usage

```go
import (
    "github.com/alexedtionweb/niro-stream/plugin/agent"
)

// Stateless (no memory)
ag, err := agent.New(llm,
    agent.WithModel("gpt-4o"),
    agent.WithSystemPrompt("You are a helpful assistant."),
)
// One turn
result, err := ag.Run(ctx, "session-1", "What is 2+2?")

// With memory (e.g. in-memory for dev)
mem := agent.NewInMemoryMemory()
ag, err = agent.New(llm,
    agent.WithMemory(mem),
    agent.WithSystemPrompt("You are a helpful assistant."),
    agent.WithHistoryPolicy(agent.SlidingWindow(20)),
)
result, err = ag.Run(ctx, "session-1", "Remember my name is Alice.")
// Next turn: history includes previous exchange
result, err = ag.Run(ctx, "session-1", "What is my name?")
```

- **Run(ctx, sessionID, input)** returns full response text (and runs the tool loop if a tooling component is mounted).
- **RunStream(ctx, sessionID, input)** returns a `*niro.Stream` so you can consume tokens as they arrive; history is still loaded and saved after the stream ends.

### Memory and history

- Implement **Memory** (Load, Save) for your store. Use **StatelessMemory** or `nil` for no persistence.
- **HistoryPolicy** (e.g. **SlidingWindow**(N)) trims how many messages are sent to the model; the full history can still be stored. If your Memory implements **BoundedLoader** and the policy implements **BoundedHistoryPolicy** with a cap, the runtime calls **LoadLast**(sessionID, max) so the backend can fetch only the last N messages.
- **WithMemoryRetry** configures retries for Load/Save (default 3 attempts, exponential backoff). Make Save idempotent for safe retries.

### Components and peers

- **WithComponent(c)** registers a component; at **New**, each component is registered with the agent’s **ComponentHost**, started, and **Apply(rt)** is called so it can attach tools or middleware to the Agent.
- **ToolingComponent** (from the agent package) attaches a **tools.Toolset** and optional **ToolExecutor** to the agent so **Run** / **RunStream** use the tool loop automatically.
- **WithPeer(peer)** registers a **Peer** (Name, Ask(ctx, sessionID, input)). Use **CallPeer(ctx, peerName, sessionID, input)** from inside the agent (or from an Orchestrator step) to delegate to another agent.

### Orchestrator (declarative steps)

The **Orchestrator** runs an **AgentDefinition**: a list of steps (JSON or Go structs). Steps run in order; each step’s input is **Step.Input** if set, otherwise the previous step’s output.

- **Step types**: **"llm"** (agent.Run), **"tool"** (Toolset.ExecuteCall), **"peer"** (CallPeer).
- Use this for fixed workflows (e.g. “call LLM → call tool → call peer”) without writing a custom loop.

```go
def := &agent.AgentDefinition{
    Name: "helper",
    Steps: []agent.Step{
        {Type: "llm", Input: "Summarize the user request in one line."},
        {Type: "tool", ToolName: "fetch_data", ToolArgs: []byte(`{"id":"123"}`)},
        {Type: "llm"}, // input = previous step output
    },
}
orch := agent.NewOrchestrator(ag, toolset)
out, err := orch.RunDefinition(ctx, sessionID, def)
```

- **LoadAgentDefinition(path)** loads from a JSON file; see **AgentDefinition** and **Step** in `plugin/agent`.

### Relation to the DSL plugin

- **plugin/agent**: programmatic API (Agent, Memory, Orchestrator, components, peers).
- **plugin/dsl**: parses and compiles **JSON** agent and workflow definitions and runs them (including multi-step and multi-agent flows); it can use the same concepts (agents, tools, steps) from config. Use the agent package when you build agents in Go; use the DSL when you drive behavior from JSON/YAML.

---

## 12. DSL and workflow (JSON)

Agents and workflows can be defined entirely in JSON. Two formats exist:

1. **DSL agent definition** (`plugin/dsl`): one JSON with **tools**, **schemas**, and **agents**. Each agent has model, prompt (Go template), tools (with optional when/unless expr), tool_choice, output, limits. Tools can be type **code** (handlers in Go), **http** (URL, method, headers, cases, retry), or **handoff** (route to another agent/workflow). Parse with `ParseDSL` / `ParseDSLFile`, then `Validate` and `Compile(opts)` to get a `NiroDefinition` and `Runner`.
2. **Workflow definition** (`plugin/dsl`): separate JSON describing how agents are composed — **fan** (parallel), **race** (first wins), **sequence** (chain), or **fan_then** (parallel then synthesizer agent). Single-workflow form: top-level `type` + `agents`; multi-workflow form: **workflows** map with nodes that can set **agent**, **agents**, and for **fan_then** **then_agent**. Parse with `ParseWorkflow`, validate with `ValidateWorkflow(wf, agentNames)`, compile with `CompileWorkflow(agentNames)`.
3. **Agent definition for Orchestrator** (`plugin/agent`): JSON with **name**, **description**, and **steps**. Each step: **type** `"llm"` | `"tool"` | `"peer"`, optional **input** (or previous output), and for tool/peer **tool_name**/**tool_args** or **peer_name**. Load with `LoadAgentDefinition(path)` and run with `Orchestrator.RunDefinition(ctx, sessionID, def)`.

**Full field-by-field reference:** every key, type, required/optional, and allowed values are documented in **[DSL_JSON_REFERENCE.md](DSL_JSON_REFERENCE.md)** — agent config (model_config, prompt, tools, output, limits), tool specs (code, http, handoff), workflow nodes (single vs multi, fan/race/sequence/fan_then), RunContext (templates and when/unless expr), and Orchestrator steps.

---

## 13. Configuration

- **API keys**: Env vars only; never commit.
- **Model**: `Request.Model` or provider default.
- **Token limits**: `Options.MaxTokens` per use case. For Gemini 2.5 thinking models set `Options.ThinkingBudget` (e.g. 0 to disable) to avoid truncated output.
- **Finish reason**: After stream ends use `stream.Response().FinishReason`. Constants: `niro.FinishReasonStop`, `niro.FinishReasonContentFilter`, etc. Use `niro.IsRefusalOrBlock(reason)` for empty/blocked responses.

---

## 14. Testing

- Mock provider with `niro.ProviderFunc`.
- Call `req.Validate()` in tests.
- Use small or zero buffer to catch ordering bugs.

---

## 15. Checklist

1. `go get github.com/alexedtionweb/niro-stream` + one provider.
2. Call `provider.Generate(ctx, req)`, iterate `stream.Next(ctx)`, check `stream.Err()` and `stream.Usage()`.
3. Optionally: `req.Validate()`, runtime + hook, output.Route, tools.NewToolLoop, middleware, plugin/agent for stateful agents.
4. Handle errors with `niro.IsRetryable` / `IsRateLimited` / `IsAuthError` and `stream.Response().FinishReason`.

See [README.md](../README.md), [ARCHITECTURE.md](../ARCHITECTURE.md), and [pkg.go.dev](https://pkg.go.dev/github.com/alexedtionweb/niro-stream).
