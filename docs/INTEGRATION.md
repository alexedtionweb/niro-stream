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

## 11. Configuration

- **API keys**: Env vars only; never commit.
- **Model**: `Request.Model` or provider default.
- **Token limits**: `Options.MaxTokens` per use case. For Gemini 2.5 thinking models set `Options.ThinkingBudget` (e.g. 0 to disable) to avoid truncated output.
- **Finish reason**: After stream ends use `stream.Response().FinishReason`. Constants: `niro.FinishReasonStop`, `niro.FinishReasonContentFilter`, etc. Use `niro.IsRefusalOrBlock(reason)` for empty/blocked responses.

---

## 12. Testing

- Mock provider with `niro.ProviderFunc`.
- Call `req.Validate()` in tests.
- Use small or zero buffer to catch ordering bugs.

---

## 13. Checklist

1. `go get github.com/alexedtionweb/niro-stream` + one provider.
2. Call `provider.Generate(ctx, req)`, iterate `stream.Next(ctx)`, check `stream.Err()` and `stream.Usage()`.
3. Optionally: `req.Validate()`, runtime + hook, output.Route, tools.NewToolLoop, middleware.
4. Handle errors with `niro.IsRetryable` / `IsRateLimited` / `IsAuthError` and `stream.Response().FinishReason`.

See [README.md](../README.md), [ARCHITECTURE.md](../ARCHITECTURE.md), and [pkg.go.dev](https://pkg.go.dev/github.com/alexedtionweb/niro-stream).
