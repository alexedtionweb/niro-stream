# Niro

**Streaming-first LLM runtime for Go.**

[![Go Reference](https://pkg.go.dev/badge/github.com/alexedtionweb/niro-stream.svg)](https://pkg.go.dev/github.com/alexedtionweb/niro-stream)
[![Go Report Card](https://goreportcard.com/badge/github.com/alexedtionweb/niro-stream)](https://goreportcard.com/report/github.com/alexedtionweb/niro-stream)

---

Niro is a high-performance, streaming-native runtime for building real-time AI systems in Go. Voice agents, telephony pipelines, streaming chat, tool calling, multimodal, parallel orchestration — with millisecond-level control over every frame of data.

**This is not LangChain for Go.** There are no chains, no prompt templates, no document loaders, no vector store abstractions. Niro is a runtime for continuous intelligence — closer to `net/http` than a notebook framework.

## Why Niro

Most LLM frameworks treat streaming as an afterthought. Niro inverts this: **streaming is the primitive**. Concurrency is a first-class design goal, not an addon.

|               | LangChain-style   | Niro                           |
| ------------- | ----------------- | ------------------------------ |
| Primary model | Request/Response  | Streaming                      |
| Data unit     | String / Document | Frame (multimodal)             |
| Composition   | Chain of calls    | Pipeline of streams            |
| Concurrency   | None              | Fan / Race / Sequence built-in |
| Backpressure  | None              | Bounded channels               |
| Telemetry     | Plugin            | Hook interface (core)          |
| Providers     | HTTP wrappers     | Official SDK-backed            |
| Target        | Notebooks         | Production systems             |

### Design Principles

1. **Streaming-first** — not streaming-compatible
2. **Concurrency as core strength** — Fan, Race, Sequence out of the box
3. **SDK-backed providers** — OpenAI, Anthropic, Google, Bedrock via official SDKs
4. **Observable by default** — Hook interface for telemetry at every stage
5. **Minimal abstractions** — maximum control
6. **Zero magic** — no reflection, no hidden state, no globals
7. **Low allocations** — tagged-union Frame, value types on the hot path
8. **Go idiomatic** — `context.Context`, interfaces, channels

## Quick Start

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
    llm := openai.New(os.Getenv("OPENAI_API_KEY"))

    stream, err := llm.Generate(ctx, &niro.Request{
        Model:        "gpt-4o",
        SystemPrompt: "You are a helpful assistant. Be concise.",
        Messages:     []niro.Message{niro.UserText("Explain Go channels in 3 sentences.")},
        Options:      niro.Options{MaxTokens: 256, Temperature: niro.Temp(0.7)},
    })
    if err != nil {
        fmt.Fprintf(os.Stderr, "error: %v\n", err)
        os.Exit(1)
    }

    for stream.Next(ctx) {
        fmt.Print(stream.Frame().Text)
    }
    fmt.Println()

    // Usage is accumulated automatically from the stream
    usage := stream.Usage()
    fmt.Fprintf(os.Stderr, "tokens: in=%d out=%d\n", usage.InputTokens, usage.OutputTokens)
}
```

Tokens arrive as they're generated. Usage is tracked silently. No buffering. No callbacks. Just a stream.

## Providers

Niro uses a **plugin model**: the core (`github.com/alexedtionweb/niro-stream`) has **zero external dependencies**. Each SDK-backed provider lives in its own Go module — you only `go get` what you use. No SDK you don't need ever enters your build graph.

| Provider          | Module                                                    | Install                                                          | SDK                                                                           |
| ----------------- | --------------------------------------------------------- | ---------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| OpenAI            | `github.com/alexedtionweb/niro-stream/provider/openai`    | `go get github.com/alexedtionweb/niro-stream/provider/openai`    | [openai/openai-go](https://github.com/openai/openai-go)                       |
| Anthropic         | `github.com/alexedtionweb/niro-stream/provider/anthropic` | `go get github.com/alexedtionweb/niro-stream/provider/anthropic` | [anthropics/anthropic-sdk-go](https://github.com/anthropics/anthropic-sdk-go) |
| Google Gemini     | `github.com/alexedtionweb/niro-stream/provider/google`    | `go get github.com/alexedtionweb/niro-stream/provider/google`    | [googleapis/go-genai](https://github.com/googleapis/go-genai)                 |
| AWS Bedrock       | `github.com/alexedtionweb/niro-stream/provider/bedrock`   | `go get github.com/alexedtionweb/niro-stream/provider/bedrock`   | [aws-sdk-go-v2](https://github.com/aws/aws-sdk-go-v2)                         |
| OpenAI-compatible | `github.com/alexedtionweb/niro-stream/provider/compat`    | included in core (zero deps)                                     | stdlib HTTP + SSE                                                             |
| Agent plugin      | `github.com/alexedtionweb/niro-stream/plugin/agent`       | `go get github.com/alexedtionweb/niro-stream/plugin/agent`       | optional component-based agent runtime                                        |

```go
// OpenAI — go get github.com/alexedtionweb/niro-stream/provider/openai
llm := openai.New(os.Getenv("OPENAI_API_KEY"))

// Anthropic — go get github.com/alexedtionweb/niro-stream/provider/anthropic
llm := anthropic.New(os.Getenv("ANTHROPIC_API_KEY"))

// Google Gemini — go get github.com/alexedtionweb/niro-stream/provider/google
llm := google.New(ctx, os.Getenv("GOOGLE_API_KEY"))

// AWS Bedrock — go get github.com/alexedtionweb/niro-stream/provider/bedrock
llm := bedrock.New(cfg) // from aws-sdk-go-v2 config

// Any OpenAI-compatible endpoint (Ollama, vLLM, LiteLLM, etc.)
// Included in core — no extra install needed
llm := compat.New("http://localhost:11434/v1", "")
```

All providers implement the same `niro.Provider` interface. Swap providers by changing one line.

### Custom Providers

```go
mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
    s, e := niro.NewStream(0)
    go func() {
        defer e.Close()
        e.Emit(ctx, niro.TextFrame("hello from mock"))
    }()
    return s, nil
})
```

### SDK Extensibility

Every SDK provider exposes its underlying client and a `RequestHook` for raw SDK parameter access. This gives you full control without forking the provider.

**Expose underlying client:**

```go
llm := openai.New(apiKey)
client := llm.Client() // returns openai-go's Client for direct API calls
```

**Per-provider `RequestHook` — modify raw SDK params before each request:**

```go
// Provider-level hook (applied to every request)
llm := openai.New(apiKey, openai.WithRequestHook(func(p *oai.ChatCompletionNewParams) {
    p.StreamOptions = oai.F(oai.ChatCompletionStreamOptionsParam{IncludeUsage: oai.Bool(true)})
}))

// Per-request hook (via Request.Extra)
stream, err := llm.Generate(ctx, &niro.Request{
    Messages: msgs,
    Extra: openai.RequestHook(func(p *oai.ChatCompletionNewParams) {
        p.LogProbs = oai.Bool(true)
    }),
})
```

Each provider defines its own `RequestHook` type with the appropriate SDK parameter struct. See the provider packages for details.

For auth/custom transport customization per SDK:

- OpenAI / Anthropic: `WithRequestOption(...)`
- Google: `WithClientOption(...)`
- Bedrock: pass AWS `aws.Config` (credentials/region/retries) into `bedrock.New(cfg, ...)`

## Core Concepts

### Frame

The universal unit of data. A `Frame` is a tagged union — a single struct with a `Kind` discriminator. Zero allocations on the text hot path.

```go
niro.TextFrame("Hello")                                           // text token
niro.AudioFrame(pcmChunk, "audio/pcm")                           // audio
niro.ImageFrame(pngBytes, "image/png")                            // image
niro.ToolCallFrame(&niro.ToolCall{ID: "1", Name: "fn", Args: j})  // tool call
niro.UsageFrame(&niro.Usage{InputTokens: 10, OutputTokens: 50})   // usage report
```

### Stream & Emitter

A `Stream` is a backpressure-aware, cancellable sequence of Frames. An `Emitter` is the write side.

```go
stream, emitter := niro.NewStream(16) // buffered channel

go func() {
    defer emitter.Close()
    emitter.Emit(ctx, niro.TextFrame("hello"))
    emitter.Emit(ctx, niro.TextFrame(" world"))
}()

for stream.Next(ctx) {
    fmt.Print(stream.Frame().Text)
}
```

**Usage auto-accumulation**: KindUsage frames are consumed silently by `stream.Next()` and accumulated in `stream.Usage()`. Providers emit them; your application reads the totals after streaming.

**ResponseMeta**: Providers set model name, finish reason, and response ID via `Emitter.SetResponse()`. Access it after streaming with `stream.Response()`.

### Processor & Pipeline

Transform streams with composable stages:

```go
pipeline := niro.Pipe(
    niro.TextOnly(),
    niro.Map(func(f niro.Frame) niro.Frame {
        f.Text = strings.ToUpper(f.Text)
        return f
    }),
    niro.Tap(func(f niro.Frame) { log.Printf("token: %q", f.Text) }),
).WithBuffer(32)

out := pipeline.Run(ctx, inputStream)
```

Built-in: `Map`, `Filter`, `Tap`, `TextOnly`, `PassThrough`, `Accumulate`. Or implement `Processor` directly.

Each stage runs in its own goroutine, connected by bounded channels. Backpressure propagates naturally.

## Orchestration

The core differentiator: concurrent LLM workflow primitives.

### Fan — Parallel Merge

Run N generations concurrently, merge all frames into one stream:

```go
stream := niro.Fan(ctx,
    func(ctx context.Context) (*niro.Stream, error) {
        return llm.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("What is Go?")}})
    },
    func(ctx context.Context) (*niro.Stream, error) {
        return llm.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("What is Rust?")}})
    },
)
```

Use cases: parallel tool calls, multi-model ensembles, scatter-gather.

### Race — First Wins

Send the same request to multiple providers; take the fastest response:

```go
text, usage, err := niro.Race(ctx,
    func(ctx context.Context) (*niro.Stream, error) {
        return openaiLLM.Generate(ctx, req)
    },
    func(ctx context.Context) (*niro.Stream, error) {
        return anthropicLLM.Generate(ctx, req)
    },
)
```

Losers are canceled immediately. Use for latency hedging and speculative execution.

### Sequence — Chained Generations

Each step receives the text output of the previous:

```go
stream, err := niro.Sequence(ctx,
    func(ctx context.Context, _ string) (*niro.Stream, error) {
        return llm.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("Write a haiku about Go")}})
    },
    func(ctx context.Context, haiku string) (*niro.Stream, error) {
        return llm.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("Critique this: " + haiku)}})
    },
)
```

Build multi-step refinement pipelines with zero boilerplate.

## Tool Calling

Tool calls are first-class streaming citizens:

```go
stream, _ := llm.Generate(ctx, &niro.Request{
    Messages: messages,
    Tools: []niro.Tool{{
        Name:        "get_weather",
        Description: "Get current weather",
        Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
    }},
})

for stream.Next(ctx) {
    f := stream.Frame()
    switch f.Kind {
    case niro.KindText:
        fmt.Print(f.Text)
    case niro.KindToolCall:
        result := executeTool(f.Tool)
        messages = append(messages, niro.ToolMessage(f.Tool.ID, result))
    }
}
```

See [examples/tools/main.go](examples/tools/main.go) for a complete tool-calling loop.

## Structured Output (JSON Schema → Typed)

Use JSON Schema to constrain model output and decode it into a typed struct.

### Final typed output

```go
type Weather struct {
    City  string `json:"city"`
    TempF int    `json:"temp_f"`
}

schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"},"temp_f":{"type":"integer"}},"required":["city","temp_f"]}`)

result, resp, usage, err := niro.GenerateStructured[Weather](ctx, llm, &niro.Request{
    Messages: []niro.Message{niro.UserText("Weather in NYC?")},
}, schema)
```

### Streaming partial + final output

```go
ss, err := niro.StreamStructured[Weather](ctx, llm, &niro.Request{
    Messages: []niro.Message{niro.UserText("Weather in NYC?")},
}, schema)
if err != nil { /* handle */ }

for ss.Next(ctx) {
    ev := ss.Event()
    if ev.Partial != nil {
        // partial valid JSON (may update as stream progresses)
    }
    if ev.Final != nil {
        // final typed output
    }
}
if err := ss.Err(); err != nil {
    // handle error
}
```

## Hooks — Telemetry & Observability

Every generation is observable through the `Hook` interface:

```go
type Hook interface {
    OnGenerateStart(ctx context.Context, info GenerateStartInfo) context.Context
    OnGenerateEnd(ctx context.Context, info GenerateEndInfo)
    OnFrame(ctx context.Context, f Frame) error
    OnToolCall(ctx context.Context, call ToolCall)
    OnToolResult(ctx context.Context, result ToolResult, elapsed time.Duration)
    OnError(ctx context.Context, err error)
}
```

Implement for Langfuse, Datadog, OpenTelemetry, cost tracking, or custom logging. Embed `niro.NoOpHook` to implement only the methods you care about. Compose multiple hooks with `niro.Hooks(h1, h2, h3)`.

Wire it via Runtime:

```go
rt := niro.NewRuntime(llm).
    WithHook(myHook).
    WithPipeline(myPipeline)

stream, err := rt.Generate(ctx, req)
```

## Error Handling & Validation

Niro provides semantic error types and request validation for robust error handling.

### Request Validation

Validate requests before invoking a provider:

```go
req := &niro.Request{
    Model: "gpt-4o",
    Messages: []niro.Message{niro.UserText("hello")},
    ResponseFormat: "json_schema",
    ResponseSchema: schema,
    Options: niro.Options{Temperature: niro.Temp(0.7)},
}

if err := req.Validate(); err != nil {
    fmt.Printf("validation error: %v (code: %d)\n", err.Message, err.Code)
}
```

Checks: non-empty messages, valid ResponseFormat, ResponseSchema validity (if json_schema), parameter ranges (Temperature ∈ [0, 2.0], TopP ∈ [0, 1.0], etc.), tool definitions, and more.

### Error Types & Semantic Handling

Errors are typed for proper handling:

```go
// Check error category
if niro.IsRetryable(err) {
    // Safe to retry
}
if niro.IsRateLimited(err) {
    // Rate limit — use backoff
}
if niro.IsAuthError(err) {
    // Invalid credentials — don't retry
}
if niro.IsTimeout(err) {
    // Timeout — may retry with longer deadline
}

// Error chaining
err := niro.WrapError(niro.ErrCodeProviderError, "OpenAI failed", underlying)
err.WithProvider("openai").WithRequestID("req_123")
```

Error codes: InvalidRequest (400), AuthenticationFailed (401), ModelNotFound (404), RateLimited (429), ProviderError (500), ServiceUnavailable (503), Timeout (504), and Niro-specific codes.

## Retry & Backoff

Automatic retry with exponential backoff for transient failures:

```go
config := niro.RetryConfig{
    MaxAttempts: 5,
    Backoff: niro.ExponentialBackoff{
        InitialDelay: 100 * time.Millisecond,
        Multiplier:   2.0,
        MaxDelay:     10 * time.Second,
        Jitter:       true, // avoid thundering herd
    },
    ShouldRetry: niro.IsRetryable, // only retry transient errors
    OnRetry: func(attempt int, err error) {
        log.Printf("Retry %d: %v", attempt, err)
    },
}

provider := niro.NewRetryProvider(llm, config)
stream, err := provider.Generate(ctx, req)
```

Works with context cancellation and respects deadlines. Only retries errors marked as retryable (429, 503, 504, stream errors).

## Timeouts & Tracing

### Timeouts

Enforce generation timeouts:

```go
provider := niro.NewTimeoutProvider(llm, 5*time.Minute)
ctx, cancel := niro.WithGenerationTimeout(context.Background(), 5*time.Minute)
defer cancel()

stream, err := provider.Generate(ctx, req)
```

### Request Tracing

Automatic request ID generation and propagation:

```go
// Generate unique request ID
requestID := niro.GenerateRequestID() // "req_<random>"

// Inject trace context
trace := niro.TraceContext{
    RequestID: requestID,
    UserID:    "user123",
    SessionID: "session456",
}
ctx = niro.WithTraceContext(ctx, trace)

// Use TracingProvider to auto-inject trace context
provider := niro.NewTracingProvider(llm)
stream, err := provider.Generate(ctx, req)

// Retrieve in hooks for logging
trace := niro.GetTraceContext(ctx)
fmt.Printf("Request: %s (user: %s)", trace.RequestID, trace.UserID)
```

## Cost Tracking

Track generation costs in real-time using the global pricing registry:

```go
// Use default pricing (auto-initialized with 2025 rates)
cost := niro.CalculateCost("openai", "gpt-4o", usage)
fmt.Printf("Cost: $%.4f (%d in, %d out)\n", cost.TotalCost, usage.InputTokens, usage.OutputTokens)

// Configure custom pricing
registry := niro.GetPricingRegistry()
registry.Set("my-provider", "my-model", &niro.ModelPricing{
    InputCostPer1M:  0.001,
    OutputCostPer1M: 0.002,
})

// Use in hooks for cost accumulation
hook := &MyHook{
    onEnd: func(ctx context.Context, info niro.GenerateEndInfo) {
        cost := info.Cost
        totalCost += cost.TotalCost
        log.Printf("Generated %d tokens for $%.4f", info.Usage.TotalTokens, cost.TotalCost)
    },
}
```

Built-in pricing for OpenAI (GPT-4, GPT-3.5), Anthropic (Claude 3.5 Sonnet, Opus), Google Gemini, and AWS Bedrock.

## Tool Execution

Automatic tool calling with loop management:

```go
executor := niro.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
    switch name {
    case "weather":
        return getWeather(args)
    case "calculator":
        return calculate(args)
    default:
        return "", fmt.Errorf("unknown tool %q", name)
    }
})

loop := niro.NewToolLoop(executor, 5) // max 5 rounds
stream, err := loop.GenerateWithTools(ctx, llm, &niro.Request{
    Messages: []niro.Message{niro.UserText("What's the weather and 2+2?")},
    Tools:    []niro.Tool{ /* ... */ },
})
```

Or use a wrapping provider:

```go
provider := niro.NewStreamWithToolHandling(llm, executor, 5)
stream, err := provider.Generate(ctx, req)
// Tool calls handled automatically
```

### Smart Tooling Abstraction (Toolset)

For Genkit-style tool definition + validation + hooks, use `Toolset`:

```go
type sumArgs struct {
    A int `json:"a"`
    B int `json:"b"`
}

toolset := niro.NewToolset()

sumTool, err := niro.NewToolDefinitionAny(
    "sum",
    "Add two integers",
    map[string]any{
        "type": "object",
        "properties": map[string]any{
            "a": map[string]any{"type": "integer"},
            "b": map[string]any{"type": "integer"},
        },
        "required": []string{"a", "b"},
    },
    func(ctx context.Context, raw json.RawMessage) (any, error) {
        var in sumArgs
        if err := niro.JSONUnmarshal(raw, &in); err != nil {
            return nil, err
        }
        return map[string]int{"sum": in.A + in.B}, nil
    },
)
if err != nil { /* handle */ }

toolset.MustRegister(sumTool)

provider := niro.NewToolingProvider(
    llm,
    toolset,
    niro.DefaultToolStreamOptions(),
)

stream, err := provider.Generate(ctx, &niro.Request{
    Messages: []niro.Message{niro.UserText("What is 20+22?")},
})
```

What this adds automatically:

- Tool schema validation for call arguments
- Tool execution lifecycle hooks (`OnToolValidate`, `OnToolExecuteStart`, `OnToolExecuteEnd`)
- Tool definitions mapped to provider-native `Request.Tools`
- Tool results fed back into subsequent turns inside the loop

## Production Composition

Combine multiple wrappers for a production-ready provider:

```go
provider := niro.ComposedProvider(
    baseProvider,
    5 * time.Minute,                          // timeout
    &niro.DefaultRetryConfig(),                // retry
)
// Adds tracing, timeout, and retry all at once
```

## Multimodal

Messages carry mixed content — text, images, audio, URLs:

```go
msg := niro.Multi(niro.RoleUser,
    niro.TextPart("What's in this image?"),
    niro.ImagePart(pngBytes, "image/png"),
    niro.ImageURLPart("https://example.com/photo.jpg", "image/jpeg"),
)
```

Streams carry interleaved text, tool calls, usage, and control signals. No separate APIs for different modalities.

## Performance

- **Zero-dependency core**: `github.com/alexedtionweb/niro-stream` has no external imports — only the Go stdlib.
- **Frame**: Tagged union (~80B value type). Text tokens: zero allocations beyond the string header.
- **Stream**: `chan Frame` with `sync/atomic` error propagation. No mutexes on the read path.
- **Pipeline**: One goroutine per stage, bounded channels for backpressure.
- **Providers**: Separate Go modules — your build only includes SDKs you use.
- **Target**: First token in <100ms over the full pipeline (network permitting).

Run benchmarks yourself:

```bash
go test -bench=. -benchmem ./...
```

## Production Infrastructure

Niro ships with production-grade infrastructure for high-concurrency deployments (millions of concurrent calls).

### BytePool — Zero-Alloc Media

`BytePool` eliminates per-frame `[]byte` allocations for audio, image, and video data using size-class `sync.Pool` buckets (4KB, 64KB, 1MB).

```go
pool := niro.DefaultBytePool // process-wide pool

// Provider emits pooled frames:
frame := niro.AudioFramePooled(pool, pcmChunk, "audio/pcm")

// Consumer returns buffer when done:
pool.Put(frame.Data)
```

Benchmark: **~60ns** per Get/Put cycle, **1 alloc** (pointer indirection). Scales linearly under parallel load.

### Transport — Connection Pooling & Keep-Alive

`Transport()` returns an optimized `*http.Transport` tuned for LLM API traffic: aggressive keep-alive, TLS session resumption, large idle pool, HTTP/2 negotiation.

```go
// Use the process-wide default (recommended):
client := niro.DefaultHTTPClient

// Or create with custom options:
client := niro.HTTPClient(&niro.TransportOptions{
    MaxIdleConnsPerHost: 50,
    IdleConnTimeout:     5 * time.Minute,
})

// Pass to any provider:
llm := compat.New(url, key, compat.WithClient(client))
```

Defaults: GOMAXPROCS×64 idle connections, GOMAXPROCS×16 per host, 120s idle timeout, TLS 1.2+, 64KB write / 32KB read buffers.

### Cache — LRU Response Cache

Thread-safe, sharded (64 shards) LRU cache with TTL for caching identical LLM requests.

```go
cache := niro.NewCache(niro.CacheOptions{
    MaxEntries: 10_000,
    TTL:        5 * time.Minute,
})
provider := cache.Wrap(llm) // transparent caching provider

stream, _ := provider.Generate(ctx, req)  // first call: miss → provider
stream, _ := provider.Generate(ctx, req)  // same request: hit → cached replay
```

Benchmark: **~1.6μs** per cache hit. Lock-free reads via atomic counters. `cache.Stats()` returns hit/miss/rate.

### Registry — Named Provider Routing

`Registry` manages named providers for runtime lookup, multi-provider deployments, and A/B routing.

```go
reg := niro.NewRegistry()
reg.Register("openai", openaiProvider)
reg.Register("anthropic", anthropicProvider)
reg.Register("fast", cache.Wrap(openaiProvider))

// Route by name at request time:
stream, err := reg.Generate(ctx, "openai", req)

// List available providers:
names := reg.Names()  // ["anthropic", "fast", "openai"]
```

Benchmark: **0 allocs, ~34ns** per lookup. RWMutex-protected, safe for concurrent registration and lookup.

### Multi-Tenancy — Runtime Client Selection

Use `MultiTenantProvider` to select provider/client at request time.

```go
reg := niro.NewRegistry()
reg.Register("tenant-a-openai", openaiA)
reg.Register("tenant-b-openai", openaiB)
reg.Register("tenant-c-bedrock", bedrockC)

router := niro.NewMultiTenantProvider(
    reg,
    niro.WithDefaultClient("tenant-a-openai"),
)

// Per-request selection
stream, err := router.Generate(ctx, &niro.Request{
    Client:   "tenant-c-bedrock",
    Messages: []niro.Message{niro.UserText("hello")},
})
```

You can also set the client in context:

```go
ctx = niro.WithClient(ctx, "tenant-b-openai")
stream, err := router.Generate(ctx, &niro.Request{Messages: msgs})
```

Per-client customization is supported via mutators:

```go
router := niro.NewMultiTenantProvider(reg,
    niro.WithClientMutator("tenant-c-bedrock", func(ctx context.Context, req *niro.Request) error {
        req.Extra = bedrock.Extras{
            InferenceProfile: "arn:aws:bedrock:us-west-2:123456789012:inference-profile/my-profile",
        }
        return nil
    }),
)
```

### AWS Bedrock Inference Profiles

Bedrock supports default and per-request inference profile targeting.

```go
llm := bedrock.New(cfg,
    bedrock.WithInferenceProfile("arn:aws:bedrock:us-west-2:123456789012:inference-profile/team-prod"),
)

// Override per request:
stream, err := llm.Generate(ctx, &niro.Request{
    Messages: []niro.Message{niro.UserText("status summary")},
    Extra: bedrock.Extras{
        InferenceProfile: "arn:aws:bedrock:us-west-2:123456789012:inference-profile/team-blue",
        Hook: func(in *bedrockruntime.ConverseStreamInput) {
            // Optional raw SDK customization
        },
    },
})
```

## Agent Plugin (Optional Module)

Core stays agent-agnostic. Agent behavior (agent-to-agent, memory, MCP memory adapters) lives in the optional plugin module.

```go
import "github.com/alexedtionweb/niro-stream/plugin/agent"

mem := agent.NewInMemoryMemory()

rt, err := agent.New(
    llm,
    agent.WithMemory(mem),
    agent.WithComponent(&agent.ToolingComponent{Toolset: toolset}),
)
if err != nil { /* handle */ }

_ = rt.Start(ctx)
defer rt.Close()

out, err := rt.Run(ctx, "session-1", "plan trip to tokyo")
fmt.Println(out.Text)
```

`agent.Runtime` also supports peer calls via `WithPeer(...)` and `CallPeer(...)` for agent-to-agent workflows.

### Object Pools

`sync.Pool`-backed pools for hot-path objects:

```go
u := niro.GetUsage()         // 0 allocs, ~21ns
defer niro.PutUsage(u)

m := niro.GetResponseMeta()  // 0 allocs, ~27ns
defer niro.PutResponseMeta(m)
```

### JSON Backend (Configurable)

Niro allows swapping the JSON implementation globally (same compatible set as Fiber):

- encoding/json (stdlib)
- github.com/goccy/go-json
- github.com/bytedance/sonic
- github.com/segmentio/encoding/json
- github.com/json-iterator/go

```go
niro.SetJSON(&niro.JSONLibrary{
    Marshal:   json.Marshal,
    Unmarshal: json.Unmarshal,
    Valid:     json.Valid,
    NewEncoder: func(w io.Writer) niro.JSONEncoder {
        return json.NewEncoder(w)
    },
    NewDecoder: func(r io.Reader) niro.JSONDecoder {
        return json.NewDecoder(r)
    },
})
```

## Examples

| Example                               | Description                                 |
| ------------------------------------- | ------------------------------------------- |
| [chat](examples/chat/main.go)         | Streaming chat with provider selection      |
| [tools](examples/tools/main.go)       | Tool-calling loop with automatic round-trip |
| [parallel](examples/parallel/main.go) | Fan, Race, Sequence orchestration           |
| [pipeline](examples/pipeline/main.go) | Processing pipeline with hooks              |

## Requirements

- Go 1.23+

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full design document covering Frame internals, Stream lifecycle, provider adapter patterns, orchestration execution model, and Hook integration.

## Roadmap

Designed for forward compatibility:

- [ ] Audio streaming pipelines (STT → LLM → TTS)
- [ ] Duplex pipelines (bidirectional streams)
- [ ] Tool execution graphs (automatic dispatch + re-invoke)
- [ ] Realtime agent loops with interruption
- [ ] Provider middleware (retries, rate limiting, fallback chains)
- [ ] WASM edge runtime support

None require breaking changes to the core.

## License

MIT
