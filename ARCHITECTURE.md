# Ryn Architecture

This document describes the internal architecture of Ryn — the streaming-first LLM runtime for Go.

## Overview

Ryn's architecture is built around a single principle: **data flows as a stream of Frames through a pipeline of Processors**. There are no request/response boundaries inside the system.

```
┌─────────────────────────────────────────────────────────────┐
│                        Ryn Runtime                          │
│                                                             │
│  ┌──────────┐     ┌────────────────────────┐    ┌────────┐ │
│  │ Provider  │────▶│       Pipeline         │───▶│ Output │ │
│  │ (LLM)    │     │ [Proc] → [Proc] → ...  │    │ Stream │ │
│  └──────────┘     └────────────────────────┘    └────────┘ │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

## Core Runtime Model

The runtime is intentionally thin. It composes a **Provider** (LLM backend) with a **Pipeline** (processing chain). No hidden state, no background workers, no magic.

```go
Runtime = Provider + Pipeline
```

A Provider generates a Stream from a Request. A Pipeline transforms a Stream through Processors. The user controls the lifecycle via `context.Context`.

## The Frame

Frame is the fundamental unit of data. It is a **tagged union** — a single struct with a `Kind` discriminator:

```
┌─────────────────────────────────────────────────────┐
│                      Frame (~80B)                    │
├──────────┬──────────────────────────────────────────┤
│ Kind     │ Discriminator (uint8)                     │
├──────────┼──────────────────────────────────────────┤
│ Text     │ string — token text (most common)         │
│ Data     │ []byte — audio/image/video binary         │
│ Mime     │ string — media type for Data              │
│ Tool     │ *ToolCall — tool invocation               │
│ Result   │ *ToolResult — tool result                 │
│ Signal   │ Signal — control (flush, eot, abort)      │
└──────────┴──────────────────────────────────────────┘
```

**Design choice**: A fat struct instead of an interface.

Why:

- No interface boxing on the hot path (text tokens)
- Value semantics — passed through `chan Frame`, no heap escapes for text
- Cache-friendly — all fields inline
- For the common case (text tokens), only `Kind` and `Text` are populated (~25 bytes of actual data)

The Frame is the universal carrier. A single Stream can carry interleaved text, audio, tool calls, and control signals. This is critical for multimodal pipelines.

## Stream & Emitter

The Stream/Emitter pair is a **unidirectional pipe** built on Go channels:

```
Emitter ──── chan Frame ────▶ Stream
(write)     (backpressure)    (read)
```

```go
stream, emitter := ryn.NewStream(bufSize)
```

**Backpressure**: The channel buffer size controls how far ahead the writer can get. With `bufSize=0`, every Emit blocks until the reader calls Next. This is the correct behavior for real-time systems (telephony, voice).

**Cancellation**: Both Emit and Next respect `context.Context`. When the context is canceled, all blocked operations return immediately.

**Error propagation**: The Emitter can set an error via `Error(err)`, which stores the error atomically and closes the channel. The Stream surfaces the error after draining buffered frames:

```
Emitter: Emit(frame1) → Emit(frame2) → Error(err) → [channel closed]
                                                          │
Stream:  Next()→frame1 → Next()→frame2 → Next()→false, Err()→err
```

This guarantees no data loss: buffered frames are delivered before the error.

### Iteration Pattern

```go
for stream.Next(ctx) {
    f := stream.Frame()
    // process f
}
if err := stream.Err(); err != nil {
    // handle
}
```

This is the `bufio.Scanner` pattern — idiomatic Go, familiar to every Go developer.

## Processor

The Processor is the composable building block:

```go
type Processor interface {
    Process(ctx context.Context, in *Stream, out *Emitter) error
}
```

**Contracts**:

- Process reads from `in`, transforms, writes to `out`
- Process must not close `out` (the Pipeline does that)
- Process should return when ctx is canceled or `in` is exhausted
- Errors are propagated to the output stream

**Built-in Processors**:

```
Filter(fn)   — only forward frames matching predicate
Map(fn)      — transform each frame
Tap(fn)      — side effect (logging, metrics) without modification
TextOnly()   — forward only KindText frames
PassThrough() — forward everything unchanged
```

**Custom Processor**: Implement the interface, or use `ProcessorFunc`:

```go
ryn.ProcessorFunc(func(ctx context.Context, in *ryn.Stream, out *ryn.Emitter) error {
    for in.Next(ctx) {
        // your logic
        out.Emit(ctx, transformed)
    }
    return in.Err()
})
```

## Pipeline

A Pipeline chains Processors with **goroutine-per-stage** execution:

```
Pipeline.Run(ctx, input) → output

input ──▶ goroutine 1 ──▶ goroutine 2 ──▶ goroutine 3 ──▶ output
          [Processor A]    [Processor B]    [Processor C]
              │                 │                 │
              └── chan Frame ───┘── chan Frame ────┘
                 (buffered)        (buffered)
```

**Execution model**:

1. `Run` creates intermediate Stream/Emitter pairs between stages
2. Each Processor runs in its own goroutine
3. Channels provide natural backpressure between stages
4. Context cancellation tears down the entire pipeline

**Error cascading**:

1. Processor B errors → calls `cancel()` on the pipeline context
2. Processor A: blocked on `Emit` → ctx canceled → returns `ctx.Err()`
3. Processor C: blocked on `Next` → channel closed or ctx canceled → returns

This is clean and deterministic. No zombie goroutines.

## Provider

The Provider interface is how LLM backends plug in:

```go
type Provider interface {
    Generate(ctx context.Context, req *Request) (*Stream, error)
}
```

**Request** carries:

- `Model`: model identifier
- `Messages`: conversation history (multimodal: text + image + audio parts)
- `Tools`: available tool definitions
- `Options`: temperature, max_tokens, etc.

**Return**: A `*Stream` that emits Frames as the LLM generates them. The `error` return is for connection/setup failures only; generation errors are propagated through the Stream.

**Bring your own model**: Use `ProviderFunc` to wrap any LLM behind the interface:

```go
ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
    s, e := ryn.NewStream(16)
    go func() {
        defer e.Close()
        // call your model, emit frames
    }()
    return s, nil
})
```

### OpenAI Provider Implementation

The built-in OpenAI provider:

1. Serializes `Request` → OpenAI JSON format (with multimodal content parts)
2. Makes HTTP POST with `Accept: text/event-stream`
3. Parses SSE events with the internal `sse.Reader`
4. Emits `TextFrame` for content deltas
5. Accumulates tool call arguments across chunks, emits `ToolCallFrame` on completion
6. Emits `ControlFrame(SignalEOT)` at end of generation

stdlib-only. ~300 lines. Works with any OpenAI-compatible endpoint.

## Cancellation Model

Everything flows through `context.Context`:

```
context.WithCancel ──▶ Provider.Generate
                   ──▶ Pipeline.Run
                   ──▶ Stream.Next / Emitter.Emit
```

Canceling the context:

- Aborts the HTTP connection to the LLM provider
- Unblocks all Stream reads and Emitter writes
- Tears down all pipeline goroutines
- Is fully synchronous — when `cancel()` returns, cleanup is in progress

There is no separate "stop" mechanism. `context.Context` is the universal cancellation primitive in Go, and Ryn uses it consistently.

## Error Propagation

Errors flow **downstream through streams**, not through separate channels:

```
Provider error  → Emitter.Error(err) → Stream.Err() returns err
Processor error → returned from Process → Pipeline sets error on output stream
Context cancel  → all operations return ctx.Err()
```

The user always checks `stream.Err()` after iteration. One place. One pattern.

## Performance Considerations

### Allocation Strategy

**Hot path (text tokens)**:

- Frame is a value type (~80B), passed through `chan Frame`
- Text is a `string` (pointer + length, 16B header) — Go strings are immutable references, no copy
- No interface boxing, no heap escapes for the frame itself

**Cold path (tool calls, multimodal)**:

- `*ToolCall` and `*ToolResult` are pointer fields — only allocated when used
- `[]byte` for binary data — single allocation per chunk
- These paths are inherently slower (network I/O bound), so allocation cost is negligible

### Goroutine Lifecycle

- Pipeline: exactly N goroutines for N processors, all managed by `sync.WaitGroup`
- Provider: exactly 1 goroutine for SSE consumption
- No goroutine leaks: context cancellation + channel close guarantees termination
- No goroutine pools or worker queues — direct execution

### Latency Targets

- First token: <100ms over pipeline (network permitting)
- Per-token overhead: <1μs (channel send + receive)
- Pipeline stage overhead: ~50ns (goroutine scheduling)
- Context check: ~10ns

### sync.Pool

Not used in the initial implementation. Frame value semantics make pooling unnecessary for the common case. If profiling shows GC pressure from binary payloads (audio/image), pooling can be added for `Frame.Data` slices without API changes.

## Future Extension Points

The architecture is designed for forward compatibility. These features can be added without breaking changes:

### Audio Streaming (STT → LLM → TTS)

```
AudioFrame → [STT Processor] → TextFrame → [Provider] → TextFrame → [TTS Processor] → AudioFrame
```

The Pipeline already supports this — just implement STT and TTS as Processors. The Frame type already has `KindAudio` and `Mime` fields.

### Duplex Pipelines

Current: unidirectional `input → pipeline → output`.
Future: bidirectional with a `DuplexPipeline` that manages two pipelines (inbound + outbound) sharing a context.

### Tool Execution Graphs

Current: tool calls are emitted as frames, user executes them.
Future: `ToolExecutor` processor that automatically dispatches tool calls, feeds results back, and re-invokes the provider. Requires a looping primitive.

### Realtime Agents

Current: single-turn generate.
Future: `Agent` that manages multi-turn conversation state, automatic tool execution, and continuous streaming with interruption support.

### WASM Edge Runtimes

The core types (Frame, Stream, Processor) have no OS dependencies. They compile to WASM today. The Provider interface abstracts the LLM backend, which could be a local WASM model.

## Package Structure

```
ryn.dev/ryn/
├── doc.go            Package documentation
├── frame.go          Frame, Kind, Signal, ToolCall, ToolResult, Tool
├── message.go        Message, Part, Role — conversation model
├── stream.go         Stream, Emitter, NewStream — the core pipe
├── processor.go      Processor interface, built-in processors
├── pipeline.go       Pipeline — concurrent processor chain
├── provider.go       Provider interface, Request, Options
├── runtime.go        Runtime — thin lifecycle composer
│
├── internal/
│   └── sse/
│       └── reader.go SSE event reader (stdlib-only)
│
├── provider/
│   └── openai/
│       └── openai.go OpenAI-compatible streaming provider
│
└── examples/
    ├── chat/          Basic streaming chat
    ├── pipeline/      Processing pipeline demo
    └── tools/         Streaming tool calls
```

### Dependency Graph

```
               ryn (core)
              /     |     \
         provider  pipeline  stream
            |
    provider/openai
            |
      internal/sse
```

Zero external dependencies. The entire library is stdlib-only.
