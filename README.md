# Ryn

**Streaming-first LLM runtime for Go.**

[![Go Reference](https://pkg.go.dev/badge/ryn.dev/ryn.svg)](https://pkg.go.dev/ryn.dev/ryn)
[![Go Report Card](https://goreportcard.com/badge/ryn.dev/ryn)](https://goreportcard.com/report/ryn.dev/ryn)

---

Ryn is a high-performance, streaming-native runtime for building real-time AI systems in Go. Voice agents, telephony pipelines, streaming chat, tool calling, multimodal — with millisecond-level control over every frame of data.

**This is not LangChain for Go.** There are no chains, no prompt templates, no document loaders, no vector store abstractions. Ryn is a runtime for continuous intelligence — closer to `net/http` than a notebook framework.

## Why Ryn

Most LLM frameworks treat streaming as an afterthought — a compatibility layer on top of request/response. Ryn inverts this: **streaming is the primitive**.

|               | LangChain-style   | Ryn                 |
| ------------- | ----------------- | ------------------- |
| Primary model | Request/Response  | Streaming           |
| Data unit     | String / Document | Frame (multimodal)  |
| Composition   | Chain of calls    | Pipeline of streams |
| Backpressure  | None              | Built-in            |
| Allocations   | Heavy             | Minimal             |
| Target        | Notebooks         | Production systems  |

### Design Principles

1. **Streaming-first** — not streaming-compatible
2. **Minimal abstractions** — maximum control
3. **Zero magic** — no reflection, no hidden state, no globals
4. **Composable pipelines** — goroutine-per-stage, channel-connected
5. **Backpressure-aware** — bounded channels, context cancellation
6. **Low allocations** — value types, no interface boxing on the hot path
7. **Go idiomatic** — `context.Context`, interfaces, channels
8. **Production-first** — not notebook-first

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
        Model: "gpt-4o",
        Messages: []ryn.Message{
            ryn.Text(ryn.RoleUser, "Explain streaming in three sentences."),
        },
    })
    if err != nil {
        fmt.Fprintf(os.Stderr, "error: %v\n", err)
        os.Exit(1)
    }

    for stream.Next(ctx) {
        fmt.Print(stream.Frame().Text)
    }
    fmt.Println()
}
```

Tokens arrive as they're generated. No buffering. No callbacks. Just a stream.

## Core Concepts

### Frame

The universal unit of data. A `Frame` carries one of: text token, audio chunk, image, video frame, tool call, tool result, or control signal.

```go
// Text token (the hot path — zero allocations beyond the string)
ryn.TextFrame("Hello")

// Multimodal
ryn.ImageFrame(pngBytes, "image/png")
ryn.AudioFrame(pcmChunk, "audio/pcm")

// Tool calling
ryn.ToolCallFrame(&ryn.ToolCall{ID: "1", Name: "search", Args: args})
```

### Stream & Emitter

A `Stream` is a backpressure-aware, cancellable sequence of Frames. An `Emitter` is the write side.

```go
stream, emitter := ryn.NewStream(16) // buffered

go func() {
    defer emitter.Close()
    emitter.Emit(ctx, ryn.TextFrame("hello"))
    emitter.Emit(ctx, ryn.TextFrame(" world"))
}()

for stream.Next(ctx) {
    fmt.Print(stream.Frame().Text)
}
```

Buffer size controls backpressure:

- `0` — unbuffered, minimum latency (telephony)
- `16` — good default for streaming
- `64+` — batch throughput

### Processor

A function that reads from one stream and writes to another. The composable building block.

```go
upper := ryn.Map(func(f ryn.Frame) ryn.Frame {
    f.Text = strings.ToUpper(f.Text)
    return f
})
```

Built-in: `Map`, `Filter`, `Tap`, `TextOnly`, `PassThrough`. Or implement `Processor` directly:

```go
type Processor interface {
    Process(ctx context.Context, in *Stream, out *Emitter) error
}
```

### Pipeline

Chains Processors with goroutine-per-stage execution:

```go
pipeline := ryn.Pipe(
    ryn.TextOnly(),
    ryn.Tap(func(f ryn.Frame) { log.Printf("token: %q", f.Text) }),
    ryn.Map(transform),
)
out := pipeline.Run(ctx, inputStream)
```

### Provider

The LLM backend interface. Bring OpenAI, Anthropic, Ollama, or your own.

```go
type Provider interface {
    Generate(ctx context.Context, req *Request) (*Stream, error)
}
```

Custom provider in 5 lines:

```go
mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
    s, e := ryn.NewStream(0)
    go func() { defer e.Close(); e.Emit(ctx, ryn.TextFrame("hello")) }()
    return s, nil
})
```

## Multimodal

Ryn is multimodal-native. Messages can carry text, images, and audio in a single request:

```go
msg := ryn.Multi(ryn.RoleUser,
    ryn.TextPart("What's in this image?"),
    ryn.ImagePart(pngBytes, "image/png"),
)
```

Streams carry mixed frame types — text tokens alongside audio chunks, tool calls, and control signals. No separate APIs for different modalities.

## Tool Calling

Tool calls are first-class streaming citizens:

```go
stream, _ := llm.Generate(ctx, &ryn.Request{
    Messages: messages,
    Tools:    []ryn.Tool{weatherTool},
})

for stream.Next(ctx) {
    f := stream.Frame()
    switch f.Kind {
    case ryn.KindText:
        fmt.Print(f.Text)
    case ryn.KindToolCall:
        result := executeTool(f.Tool)
        // Feed result back for next turn
    }
}
```

See [examples/tools](examples/tools/main.go) for a complete example.

## OpenAI Provider

The built-in OpenAI provider works with any OpenAI-compatible API:

```go
// OpenAI
llm := openai.New(apiKey)

// Azure OpenAI
llm := openai.New(apiKey, openai.WithBaseURL("https://your-resource.openai.azure.com/openai/deployments/gpt-4o"))

// Ollama
llm := openai.New("", openai.WithBaseURL("http://localhost:11434/v1"))

// Any OpenAI-compatible endpoint
llm := openai.New(apiKey, openai.WithBaseURL("https://your-endpoint/v1"))
```

stdlib-only. No external dependencies. ~300 lines.

## Performance

Ryn is designed to feel like `net/http`, not like a framework:

- **Frame**: ~80 bytes value type. Text tokens use only `Kind` + `Text` (no heap allocation beyond the string).
- **Stream**: `chan Frame` with atomic error propagation. No mutexes on the read path.
- **Pipeline**: One goroutine per stage, bounded channels for backpressure. No coordination overhead.
- **Provider**: Direct `net/http` + SSE parsing. No intermediate buffering.

Target: **first token in <100ms** over the full pipeline (network permitting).

## Examples

| Example                               | Description                              |
| ------------------------------------- | ---------------------------------------- |
| [chat](examples/chat/main.go)         | Basic streaming chat                     |
| [pipeline](examples/pipeline/main.go) | LLM output through a processing pipeline |
| [tools](examples/tools/main.go)       | Streaming tool calls                     |

## Requirements

- Go 1.22+
- No external dependencies (stdlib only)

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full design document.

## Roadmap

Designed for forward compatibility. Extension points exist for:

- [ ] Audio streaming (STT → LLM → TTS)
- [ ] Duplex pipelines (bidirectional)
- [ ] Tool execution graphs
- [ ] Realtime agent loops
- [ ] WASM edge runtimes
- [ ] Anthropic / Bedrock / Ollama providers

None of these require breaking changes to the core.

## License

MIT
