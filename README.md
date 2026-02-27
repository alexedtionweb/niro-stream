# Ryn

**Streaming-first LLM runtime for Go.**

[![Go Reference](https://pkg.go.dev/badge/ryn.dev/ryn.svg)](https://pkg.go.dev/ryn.dev/ryn)
[![Go Report Card](https://goreportcard.com/badge/ryn.dev/ryn)](https://goreportcard.com/report/ryn.dev/ryn)

---

Ryn is a high-performance, streaming-native runtime for building real-time AI systems in Go. Voice agents, telephony pipelines, streaming chat, tool calling, multimodal, parallel orchestration — with millisecond-level control over every frame of data.

**This is not LangChain for Go.** There are no chains, no prompt templates, no document loaders, no vector store abstractions. Ryn is a runtime for continuous intelligence — closer to `net/http` than a notebook framework.

## Why Ryn

Most LLM frameworks treat streaming as an afterthought. Ryn inverts this: **streaming is the primitive**. Concurrency is a first-class design goal, not an addon.

|               | LangChain-style   | Ryn                            |
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

    "ryn.dev/ryn"
    "ryn.dev/ryn/provider/openai"
)

func main() {
    ctx := context.Background()
    llm := openai.New(os.Getenv("OPENAI_API_KEY"))

    stream, err := llm.Generate(ctx, &ryn.Request{
        Model:        "gpt-4o",
        SystemPrompt: "You are a helpful assistant. Be concise.",
        Messages:     []ryn.Message{ryn.UserText("Explain Go channels in 3 sentences.")},
        Options:      ryn.Options{MaxTokens: 256, Temperature: ryn.Temp(0.7)},
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

Ryn ships with five provider implementations backed by official SDKs:

| Provider          | Package              | SDK                                                                           |
| ----------------- | -------------------- | ----------------------------------------------------------------------------- |
| OpenAI            | `provider/openai`    | [openai/openai-go](https://github.com/openai/openai-go)                       |
| Anthropic         | `provider/anthropic` | [anthropics/anthropic-sdk-go](https://github.com/anthropics/anthropic-sdk-go) |
| Google Gemini     | `provider/google`    | [google/generative-ai-go](https://github.com/google/generative-ai-go)         |
| AWS Bedrock       | `provider/bedrock`   | [aws-sdk-go-v2](https://github.com/aws/aws-sdk-go-v2)                         |
| OpenAI-compatible | `provider/compat`    | stdlib HTTP + SSE (no SDK)                                                    |

```go
// OpenAI
llm := openai.New(os.Getenv("OPENAI_API_KEY"))

// Anthropic
llm := anthropic.New(os.Getenv("ANTHROPIC_API_KEY"))

// Google Gemini
llm := google.New(ctx, os.Getenv("GOOGLE_API_KEY"))

// AWS Bedrock
llm := bedrock.New(cfg) // from aws-sdk-go-v2 config

// Any OpenAI-compatible endpoint (Ollama, vLLM, LiteLLM, etc.)
llm := compat.New("http://localhost:11434/v1", "")
```

All providers implement the same `ryn.Provider` interface. Swap providers by changing one line.

### Custom Providers

```go
mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
    s, e := ryn.NewStream(0)
    go func() {
        defer e.Close()
        e.Emit(ctx, ryn.TextFrame("hello from mock"))
    }()
    return s, nil
})
```

## Core Concepts

### Frame

The universal unit of data. A `Frame` is a tagged union — a single struct with a `Kind` discriminator. Zero allocations on the text hot path.

```go
ryn.TextFrame("Hello")                                           // text token
ryn.AudioFrame(pcmChunk, "audio/pcm")                           // audio
ryn.ImageFrame(pngBytes, "image/png")                            // image
ryn.ToolCallFrame(&ryn.ToolCall{ID: "1", Name: "fn", Args: j})  // tool call
ryn.UsageFrame(&ryn.Usage{InputTokens: 10, OutputTokens: 50})   // usage report
```

### Stream & Emitter

A `Stream` is a backpressure-aware, cancellable sequence of Frames. An `Emitter` is the write side.

```go
stream, emitter := ryn.NewStream(16) // buffered channel

go func() {
    defer emitter.Close()
    emitter.Emit(ctx, ryn.TextFrame("hello"))
    emitter.Emit(ctx, ryn.TextFrame(" world"))
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
pipeline := ryn.Pipe(
    ryn.TextOnly(),
    ryn.Map(func(f ryn.Frame) ryn.Frame {
        f.Text = strings.ToUpper(f.Text)
        return f
    }),
    ryn.Tap(func(f ryn.Frame) { log.Printf("token: %q", f.Text) }),
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
stream := ryn.Fan(ctx,
    func(ctx context.Context) (*ryn.Stream, error) {
        return llm.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("What is Go?")}})
    },
    func(ctx context.Context) (*ryn.Stream, error) {
        return llm.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("What is Rust?")}})
    },
)
```

Use cases: parallel tool calls, multi-model ensembles, scatter-gather.

### Race — First Wins

Send the same request to multiple providers; take the fastest response:

```go
text, usage, err := ryn.Race(ctx,
    func(ctx context.Context) (*ryn.Stream, error) {
        return openaiLLM.Generate(ctx, req)
    },
    func(ctx context.Context) (*ryn.Stream, error) {
        return anthropicLLM.Generate(ctx, req)
    },
)
```

Losers are canceled immediately. Use for latency hedging and speculative execution.

### Sequence — Chained Generations

Each step receives the text output of the previous:

```go
stream, err := ryn.Sequence(ctx,
    func(ctx context.Context, _ string) (*ryn.Stream, error) {
        return llm.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("Write a haiku about Go")}})
    },
    func(ctx context.Context, haiku string) (*ryn.Stream, error) {
        return llm.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("Critique this: " + haiku)}})
    },
)
```

Build multi-step refinement pipelines with zero boilerplate.

## Tool Calling

Tool calls are first-class streaming citizens:

```go
stream, _ := llm.Generate(ctx, &ryn.Request{
    Messages: messages,
    Tools: []ryn.Tool{{
        Name:        "get_weather",
        Description: "Get current weather",
        Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
    }},
})

for stream.Next(ctx) {
    f := stream.Frame()
    switch f.Kind {
    case ryn.KindText:
        fmt.Print(f.Text)
    case ryn.KindToolCall:
        result := executeTool(f.Tool)
        messages = append(messages, ryn.ToolMessage(f.Tool.ID, result))
    }
}
```

See [examples/tools](examples/tools/main.go) for a complete tool-calling loop.

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

Implement for Langfuse, Datadog, OpenTelemetry, cost tracking, or custom logging. Embed `ryn.NoOpHook` to implement only the methods you care about. Compose multiple hooks with `ryn.Hooks(h1, h2, h3)`.

Wire it via Runtime:

```go
rt := ryn.NewRuntime(llm).
    WithHook(myHook).
    WithPipeline(myPipeline)

stream, err := rt.Generate(ctx, req)
```

## Multimodal

Messages carry mixed content — text, images, audio, URLs:

```go
msg := ryn.Multi(ryn.RoleUser,
    ryn.TextPart("What's in this image?"),
    ryn.ImagePart(pngBytes, "image/png"),
    ryn.ImageURLPart("https://example.com/photo.jpg", "image/jpeg"),
)
```

Streams carry interleaved text, tool calls, usage, and control signals. No separate APIs for different modalities.

## Performance

- **Frame**: Tagged union (~80B value type). Text tokens: only `Kind` + `Text` populated — no heap allocation beyond the string.
- **Stream**: `chan Frame` with `sync/atomic` error propagation. No mutexes on the read path.
- **Pipeline**: One goroutine per stage, bounded channels for backpressure.
- **Providers**: Official SDKs with streaming APIs. No intermediate buffering.
- **Target**: First token in <100ms over the full pipeline (network permitting).

## Examples

| Example                               | Description                                 |
| ------------------------------------- | ------------------------------------------- |
| [chat](examples/chat/main.go)         | Streaming chat with provider selection      |
| [tools](examples/tools/main.go)       | Tool-calling loop with automatic round-trip |
| [parallel](examples/parallel/main.go) | Fan, Race, Sequence orchestration           |
| [pipeline](examples/pipeline/main.go) | Processing pipeline with hooks              |

## Requirements

- Go 1.22+

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full design document covering Frame internals, Stream lifecycle, provider adapter patterns, orchestration execution model, and Hook integration.

## Roadmap

Designed for forward compatibility:

- [ ] Audio streaming pipelines (STT → LLM → TTS)
- [ ] Duplex pipelines (bidirectional streams)
- [ ] Tool execution graphs (automatic dispatch + re-invoke)
- [ ] Realtime agent loops with interruption
- [ ] Structured output helpers (JSON schema → typed response)
- [ ] WASM edge runtime support

None require breaking changes to the core.

## License

MIT
