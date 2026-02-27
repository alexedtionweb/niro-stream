# Ryn Architecture

This document describes the internal architecture of Ryn — the streaming-first LLM runtime for Go.

## Overview

Ryn's architecture is built around a single principle: **data flows as a stream of Frames through a pipeline of Processors, orchestrated concurrently**. Everything — text tokens, tool calls, usage data, control signals — is a Frame in a Stream.

```
┌───────────────────────────────────────────────────────────────────┐
│                          Ryn Runtime                              │
│                                                                   │
│  ┌──────────┐    ┌────────────────────────┐    ┌──────────────┐  │
│  │ Provider  │───▶│       Pipeline         │───▶│ Output Stream │  │
│  │ (LLM SDK)│    │ [Proc] → [Proc] → ...  │    │  → Consumer  │  │
│  └──────────┘    └────────────────────────┘    └──────────────┘  │
│       │                                               │           │
│       └──────────── Hook (telemetry) ─────────────────┘           │
│                                                                   │
│  ┌───────────────────────────────────────────────────────────┐   │
│  │              Orchestration Layer                           │   │
│  │   Fan (parallel merge)  ·  Race (first wins)              │   │
│  │   Sequence (chained)                                      │   │
│  └───────────────────────────────────────────────────────────┘   │
└───────────────────────────────────────────────────────────────────┘
```

## Core Runtime Model

The runtime composes three concerns:

```
Runtime = Provider + Pipeline + Hook
```

- **Provider**: Generates a Stream from a Request (LLM backend)
- **Pipeline**: Transforms a Stream through Processors (post-processing)
- **Hook**: Observes every frame, request, and response (telemetry)

The user controls the lifecycle via `context.Context`. No hidden state, no background workers.

## The Frame

Frame is the fundamental unit of data. It is a **tagged union** — a single struct with a `Kind` discriminator:

```
┌──────────────────────────────────────────────────────────┐
│                     Frame (~80B value type)               │
├──────────┬───────────────────────────────────────────────┤
│ Kind     │ Discriminator (uint8)                          │
├──────────┼───────────────────────────────────────────────┤
│ Text     │ string — token text (most common hot path)     │
│ Data     │ []byte — audio/image/video binary              │
│ Mime     │ string — media type for Data                   │
│ Tool     │ *ToolCall — tool invocation from LLM           │
│ Result   │ *ToolResult — tool execution result            │
│ Usage    │ *Usage — token usage report                    │
│ Signal   │ Signal — control (flush, eot, abort)           │
└──────────┴───────────────────────────────────────────────┘
```

**Design choice**: A fat struct instead of an interface.

Why:

- **No interface boxing on the hot path** — text tokens (the 99% case) use only `Kind` + `Text`
- **Value semantics** — passed through `chan Frame`, minimal heap escapes
- **Cache-friendly** — all fields inline, no pointer chasing
- **Common case is cheap** — text tokens are ~25 bytes of actual data in an 80-byte struct

The Frame is the universal carrier. A single Stream can carry interleaved text, audio, tool calls, usage data, and control signals. This is critical for multimodal pipelines and for usage tracking.

### Kind Variants

| Kind             | Fields Used    | Allocation                | Hot Path      |
| ---------------- | -------------- | ------------------------- | ------------- |
| `KindText`       | `Text`         | Zero (string header only) | ✅            |
| `KindAudio`      | `Data`, `Mime` | `[]byte` slice            | No            |
| `KindImage`      | `Data`, `Mime` | `[]byte` slice            | No            |
| `KindVideo`      | `Data`, `Mime` | `[]byte` slice            | No            |
| `KindToolCall`   | `Tool`         | `*ToolCall` pointer       | No            |
| `KindToolResult` | `Result`       | `*ToolResult` pointer     | No            |
| `KindUsage`      | `Usage`        | `*Usage` pointer          | Auto-consumed |
| `KindControl`    | `Signal`       | Zero (uint8)              | No            |

## Stream & Emitter

The Stream/Emitter pair is a **unidirectional pipe** built on Go channels:

```
Emitter ──── chan Frame ────▶ Stream
(write)     (backpressure)    (read)
```

```go
stream, emitter := ryn.NewStream(bufSize)
```

### Backpressure

The channel buffer size controls how far ahead the writer can get:

- `0` — unbuffered, minimum latency (telephony, voice)
- `16` — good default for streaming text
- `32+` — batch throughput, pipeline stages

### Cancellation

Both `Emit` and `Next` respect `context.Context`. When the context is canceled, all blocked operations return immediately.

### Error Propagation

```
Emitter: Emit(frame1) → Emit(frame2) → Error(err) → [channel closed]
                                                          │
Stream:  Next()→frame1 → Next()→frame2 → Next()→false, Err()→err
```

Buffered frames are always delivered before the error. No data loss.

### Usage Auto-Accumulation

`stream.Next()` silently consumes `KindUsage` frames and accumulates them internally. They never reach the caller's iteration loop. After the stream is exhausted, `stream.Usage()` returns the totals.

This means providers can emit usage at any point in the stream (typically at the end) and consumers don't need to handle it:

```go
// Provider side (inside the goroutine):
emitter.Emit(ctx, ryn.TextFrame("Hello"))
emitter.Emit(ctx, ryn.UsageFrame(&ryn.Usage{InputTokens: 10, OutputTokens: 1}))

// Consumer side — never sees the UsageFrame:
for stream.Next(ctx) {
    // only KindText frames arrive here
}
usage := stream.Usage() // {InputTokens: 10, OutputTokens: 1}
```

### ResponseMeta

Providers set structured metadata via `Emitter.SetResponse()`:

```go
emitter.SetResponse(&ryn.ResponseMeta{
    Model:        "gpt-4o-2024-08-06",
    FinishReason: "stop",
    ID:           "chatcmpl-abc123",
    Usage:        usage,
})
```

Consumers access it via `stream.Response()` after the stream is exhausted.

### Iteration Pattern

```go
for stream.Next(ctx) {
    f := stream.Frame()
    // process f
}
if err := stream.Err(); err != nil {
    // handle error
}
```

This is the `bufio.Scanner` pattern — idiomatic Go, familiar to every Go developer.

## Processor

The Processor is the composable building block for stream transformation:

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

| Processor       | Behavior                                            |
| --------------- | --------------------------------------------------- |
| `Filter(fn)`    | Only forward frames matching predicate              |
| `Map(fn)`       | Transform each frame                                |
| `Tap(fn)`       | Side effect (logging, metrics) without modification |
| `TextOnly()`    | Forward only KindText frames                        |
| `PassThrough()` | Forward everything unchanged                        |
| `Accumulate()`  | Buffer all text, emit single concatenated frame     |

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

### Execution Model

1. `Pipe(processors...).WithBuffer(n)` creates the Pipeline
2. `Run(ctx, input)` creates intermediate Stream/Emitter pairs
3. Each Processor runs in its own goroutine
4. Channels provide natural backpressure between stages
5. Context cancellation tears down the entire pipeline
6. A `sync.WaitGroup` tracks all goroutines — no leaks

### Error Cascading

1. Processor B errors → `cancel()` on the pipeline context
2. Processor A: blocked on `Emit` → ctx canceled → returns
3. Processor C: blocked on `Next` → channel closed → returns

Clean and deterministic. No zombie goroutines.

### Buffer Sizing

Default buffer: 16. Override with `WithBuffer(n)`:

```go
ryn.Pipe(procs...).WithBuffer(64) // larger buffer for throughput
ryn.Pipe(procs...).WithBuffer(0)  // unbuffered for minimum latency
```

## Provider

The Provider interface is how LLM backends plug in:

```go
type Provider interface {
    Generate(ctx context.Context, req *Request) (*Stream, error)
}
```

### Request

```go
type Request struct {
    Model          string          // "gpt-4o", "claude-sonnet-4-5", "gemini-2.0-flash"
    SystemPrompt   string          // Convenience: prepended as system message
    Messages       []Message       // Conversation history (multimodal)
    Tools          []Tool          // Available tool definitions
    ToolChoice     ToolChoice      // auto, none, required, or specific function
    ResponseFormat string          // "", "json", "json_schema"
    ResponseSchema json.RawMessage // JSON Schema for structured output
    Options        Options         // Temperature, MaxTokens, TopP, etc.
}
```

`EffectiveMessages()` returns Messages with SystemPrompt prepended as a system message.

### Provider Implementations

All built-in providers follow the same internal pattern:

1. **Translate** `ryn.Request` → SDK-specific params (messages, tools, options)
2. **Call** the SDK's streaming method
3. **Spawn** a goroutine that reads from the SDK stream
4. **Emit** Frames: `KindText` for deltas, `KindToolCall` for completed tools, `KindUsage` for token counts
5. **Set** `ResponseMeta` with model, finish reason, response ID
6. **Close** the emitter when the SDK stream ends

#### OpenAI Provider

Uses `openai-go` (official SDK). Key details:

- Client is a **value type** (not pointer)
- Streaming via `client.Chat.Completions.NewStreaming(ctx, params)`
- `ChatCompletionAccumulator` tracks tool call argument chunks
- `JustFinishedToolCall()` detects completed tool calls
- Tool call fields: `.Id`, `.Name`, `.Arguments`
- `StreamOptions{IncludeUsage: true}` to get usage in the stream

#### Anthropic Provider

Uses `anthropic-sdk-go` (official SDK). Key details:

- Client is a **value type**
- Streaming via `client.Messages.NewStreaming(ctx, params)`
- `Message.Accumulate(event)` to build up the response
- `ContentBlockDeltaEvent` → `TextDelta` for text chunks
- Tool calls extracted from accumulated `message.Content` blocks
- System prompt is `[]TextBlockParam`, not a string

#### Google Gemini Provider

Uses `google/generative-ai-go`. Key details:

- `GenerativeModel` with `StartChat` + `SendMessageStream`
- `iterator.Done` pattern for stream exhaustion
- Parts: `genai.Text`, `genai.FunctionCall`, `genai.Blob`
- System instruction set via `model.SystemInstruction`

#### AWS Bedrock Provider

Uses `aws-sdk-go-v2` ConverseStream API. Key details:

- Event channel pattern with type switching
- `ContentBlockDeltaMemberText` / `ContentBlockDeltaMemberToolUse`
- `document.NewLazyDocument()` for tool JSON schemas
- `ConverseStreamMetadataEvent` for usage

#### Compat Provider

Raw HTTP + SSE for OpenAI-compatible endpoints:

- No SDK dependencies — stdlib `net/http` + internal `sse.Reader`
- Works with Ollama, vLLM, LiteLLM, any OpenAI-compatible API
- Accumulates streaming tool call arguments across SSE chunks
- Custom headers via `WithHeader()`

## Hook — Telemetry & Observability

The Hook interface provides observability into every stage of an LLM interaction:

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

### Lifecycle

```
OnGenerateStart(ctx, info) → ctx'
    │
    ├── OnFrame(ctx', frame) × N      [per token]
    ├── OnToolCall(ctx', call)         [if tool use]
    ├── OnToolResult(ctx', result, d)  [if tool result]
    ├── OnError(ctx', err)             [on error]
    │
OnGenerateEnd(ctx', info)              [stream exhausted]
```

### Key Design Decisions

1. **`OnGenerateStart` returns a `context.Context`** — inject trace IDs, span contexts, request-scoped values
2. **All methods are synchronous** — heavy work (network I/O) should be dispatched to background goroutines
3. **`OnFrame` can abort** — return a non-nil error to terminate the stream
4. **`NoOpHook` embed pattern** — implement only the methods you need

### Composition

```go
combined := ryn.Hooks(langfuseHook, datadogHook, costTracker)
```

`Hooks()` returns a `multiHook` that fans out to all hooks. Nil hooks are filtered. If only one non-nil hook remains, it's returned directly (no wrapper overhead).

### Integration with Runtime

```go
rt := ryn.NewRuntime(llm).WithHook(hook)
```

The Runtime wraps the provider's stream to intercept every frame and fire the hook lifecycle automatically.

## Orchestration

The orchestration layer provides concurrency primitives for complex LLM workflows. These are the core differentiator — patterns that are trivial in Go but painful in other languages.

### Fan (Parallel Merge)

```
          ┌── gen A ──▶ stream A ──┐
ctx ──────┤── gen B ──▶ stream B ──├──▶ merged stream
          └── gen C ──▶ stream C ──┘
```

- N goroutines, one per generation function
- Frames interleave in arrival order
- `sync.WaitGroup` for clean shutdown
- Any error propagates but doesn't cancel siblings
- Merged stream closes when all sources are exhausted

Use cases: parallel tool calls, multi-model ensembles, scatter-gather.

### Race (First Wins)

```
          ┌── gen A ──▶ [collecting...] ──┐
ctx ──────┤── gen B ──▶ [WINNER!]  ───────├──▶ text, usage, err
          └── gen C ──▶ [canceled] ───────┘
```

- N goroutines race to produce a complete text response
- First success cancels all others via `context.WithCancel`
- Returns collected text + usage + error
- Failed generations are retried implicitly (next result is checked)

Use cases: latency hedging, speculative execution, provider failover.

### Sequence (Chained Generations)

```
gen A("") ──▶ text A ──▶ gen B(text A) ──▶ text B ──▶ gen C(text B) ──▶ stream C
```

- Each function receives the collected text of the previous stage
- Intermediate stages collect fully; final stage returns a live stream
- Empty function list returns an empty stream

Use cases: multi-step refinement, chain-of-thought, translation chains.

## Cancellation Model

Everything flows through `context.Context`:

```
context.WithCancel ──▶ Provider.Generate
                   ──▶ Pipeline.Run
                   ──▶ Stream.Next / Emitter.Emit
                   ──▶ Orchestration (Fan/Race/Sequence)
```

Canceling the context:

- Aborts the SDK connection to the LLM provider
- Unblocks all Stream reads and Emitter writes
- Tears down all pipeline goroutines
- Cancels sibling streams in Race
- Is fully synchronous — `cancel()` returns, cleanup is in progress

There is no separate "stop" mechanism. `context.Context` is the universal cancellation primitive.

## Error Propagation

Errors flow **downstream through streams**, not through separate channels:

```
Provider error  → Emitter.Error(err) → Stream.Err() returns err
Processor error → returned from Process → Pipeline sets error on output
Context cancel  → all operations return ctx.Err()
Hook error      → OnFrame returns err → stream aborted
```

The user always checks `stream.Err()` after iteration. One place. One pattern.

## Performance Considerations

### Allocation Strategy

**Hot path (text tokens)**:

- Frame is a value type (~80B), passed through `chan Frame`
- Text is a `string` — Go strings are immutable references, no copy
- No interface boxing, minimal heap escapes

**Cold path (tool calls, usage, multimodal)**:

- `*ToolCall`, `*ToolResult`, `*Usage` are pointer fields — allocated only when used
- `[]byte` for binary data — single allocation per chunk
- These paths are inherently I/O bound, so allocation cost is negligible

### Goroutine Lifecycle

- **Pipeline**: exactly N goroutines for N processors, managed by `sync.WaitGroup`
- **Provider**: exactly 1 goroutine for SDK stream consumption
- **Fan**: N goroutines for N generation functions + 1 closer goroutine
- **Race**: N goroutines with shared `context.WithCancel`
- No goroutine leaks: context cancellation + channel close guarantees termination

### Latency Targets

- First token: <100ms over pipeline (network permitting)
- Per-token overhead: <1μs (channel send + receive)
- Pipeline stage overhead: ~50ns (goroutine scheduling)
- Hook overhead: depends on implementation (keep synchronous path fast)

## Usage Tracking

The `Usage` struct tracks token consumption with a composable `Add()` method:

```go
type Usage struct {
    InputTokens  int
    OutputTokens int
    TotalTokens  int
    Detail       map[string]int // Provider-specific (cache hits, etc.)
}
```

- Providers emit `KindUsage` frames
- `Stream.Next()` auto-accumulates them
- `Usage.Add()` merges two Usage values (Detail maps are combined)
- Orchestration primitives (Race) return Usage alongside text

## Package Structure

```
ryn.dev/ryn/
├── doc.go              Package documentation
├── frame.go            Frame, Kind, Signal, ToolCall, ToolResult, Tool, Usage
├── message.go          Message, Part, Role — conversation model
├── stream.go           Stream, Emitter, NewStream — the core pipe
├── processor.go        Processor interface, Map/Filter/Tap/TextOnly/Accumulate
├── pipeline.go         Pipeline — concurrent goroutine-per-stage chain
├── provider.go         Provider interface, Request, Options
├── hook.go             Hook interface, GenerateStartInfo/EndInfo, NoOpHook, Hooks()
├── orchestrate.go      Fan, Race, Sequence — concurrent workflow primitives
├── runtime.go          Runtime — lifecycle composer (Provider + Pipeline + Hook)
│
├── internal/
│   └── sse/
│       └── reader.go   SSE event reader (stdlib-only, for compat provider)
│
├── provider/
│   ├── openai/         OpenAI SDK-backed provider
│   │   ├── openai.go
│   │   └── encode.go
│   ├── anthropic/      Anthropic SDK-backed provider
│   │   └── anthropic.go
│   ├── google/         Google Gemini SDK-backed provider
│   │   └── google.go
│   ├── bedrock/        AWS Bedrock SDK-backed provider
│   │   └── bedrock.go
│   └── compat/         OpenAI-compatible HTTP+SSE provider
│       └── compat.go
│
└── examples/
    ├── chat/           Basic streaming chat with provider selection
    ├── tools/          Tool-calling loop with round-trip
    ├── parallel/       Fan, Race, Sequence orchestration demo
    └── pipeline/       Processing pipeline with Hook telemetry
```

### Dependency Graph

```
                  ryn (core)
                /    |    \     \
           stream pipeline hook  orchestrate
                        \
                     provider.go
                    /   |    |    \     \
             openai anthropic google bedrock compat
             (SDK)   (SDK)   (SDK)  (SDK)  (stdlib)
```

The core package (`ryn`) has zero external dependencies. Provider packages depend on their respective official SDKs.

## Future Extension Points

The architecture is designed for forward compatibility:

### Audio Streaming (STT → LLM → TTS)

```
AudioFrame → [STT Processor] → TextFrame → [Provider] → TextFrame → [TTS Processor] → AudioFrame
```

The Pipeline already supports this. Frame already has `KindAudio` and `Mime`.

### Tool Execution Graphs

Current: tool calls emitted as frames, user executes them.
Future: `ToolExecutor` processor that automatically dispatches, feeds results back, and re-invokes the provider. Requires a looping primitive in the Pipeline.

### Realtime Agents

Current: single-turn generate, manual tool loop.
Future: `Agent` that manages multi-turn state, automatic tool execution, and continuous streaming with interruption support.

### Structured Output

Current: `ResponseFormat` + `ResponseSchema` on Request.
Future: Generic helper that decodes the final text into a typed Go struct.

### Provider Middleware

Current: Hook provides observability.
Future: Provider middleware for retries, rate limiting, caching, fallback chains.
