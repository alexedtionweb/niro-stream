# Niro Architecture

This document describes the internal architecture of Niro — the streaming-first LLM runtime for Go.

## Overview

Niro's architecture is built around a single principle: **data flows as a stream of Frames through a pipeline of Processors, orchestrated concurrently**. Everything — text tokens, tool calls, usage data, control signals — is a Frame in a Stream.

```
┌───────────────────────────────────────────────────────────────────┐
│                          Niro Runtime                             │
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
│ Custom   │ *ExperimentalFrame — extension payload         │
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
| `KindCustom`     | `Custom`       | `*ExperimentalFrame`      | Optional      |
| `KindControl`    | `Signal`       | Zero (uint8)              | No            |

## Stream & Emitter

The Stream/Emitter pair is a **unidirectional pipe** built on Go channels:

```
Emitter ──── chan Frame ────▶ Stream
(write)     (backpressure)    (read)
```

```go
stream, emitter := niro.NewStream(bufSize)
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
emitter.Emit(ctx, niro.TextFrame("Hello"))
emitter.Emit(ctx, niro.UsageFrame(&niro.Usage{InputTokens: 10, OutputTokens: 1}))

// Consumer side — never sees the UsageFrame:
for stream.Next(ctx) {
    // only KindText frames arrive here
}
usage := stream.Usage() // {InputTokens: 10, OutputTokens: 1}
```

Reasoning/accounting metadata uses stable keys in `Usage.Detail`, including:

- `niro.UsageReasoningTokens`
- `niro.UsageReasoningCost`

### ResponseMeta

Providers set structured metadata via `Emitter.SetResponse()`:

```go
emitter.SetResponse(&niro.ResponseMeta{
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
niro.Pipe(procs...).WithBuffer(64) // larger buffer for throughput
niro.Pipe(procs...).WithBuffer(0)  // unbuffered for minimum latency
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
    Extra          any             // Provider-specific SDK hooks (see below)
}
```

`Options.ExperimentalReasoning` is an explicit opt-in gate for provider reasoning extensions. Unsupported providers should return an error rather than silently ignoring it.

`EffectiveMessages()` returns Messages with SystemPrompt prepended as a system message.

#### Request.Extra — Per-Request SDK Customization

The `Extra` field enables raw SDK parameter access on a per-request basis without modifying the provider. Each SDK provider checks `Extra` for its own `RequestHook` type:

```go
// OpenAI: type RequestHook func(params *oai.ChatCompletionNewParams)
// Anthropic: type RequestHook func(params *ant.MessageNewParams)
// Google: type RequestHook func(model *genai.GenerativeModel)
// Bedrock: type RequestHook func(input *bedrockruntime.ConverseStreamInput)
```

Providers ignore unrecognized `Extra` types. This pattern lets a single `niro.Request` carry provider-specific tuning without breaking the common interface.

### Provider Implementations

All SDK providers are **separate Go modules** (`github.com/alexedtionweb/niro-stream/provider/<name>`). They follow the same internal pattern:

1. **Translate** `niro.Request` → SDK-specific params (messages, tools, options)
2. **Apply hooks**: provider-level `WithRequestHook` + per-request `Request.Extra`
3. **Call** the SDK's streaming method
4. **Spawn** a goroutine that reads from the SDK stream
5. **Emit** Frames: `KindText` for deltas, `KindToolCall` for completed tools, `KindUsage` for token counts
6. **Set** `ResponseMeta` with model, finish reason, response ID
7. **Close** the emitter when the SDK stream ends

Each provider exposes:

- **`Client()`** — the underlying SDK client for direct API access
- **`RequestHook` type** — function receiving raw SDK params
- **`WithRequestHook()`** — provider option applying a hook to every request

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
combined := niro.Hooks(langfuseHook, datadogHook, costTracker)
```

`Hooks()` returns a `multiHook` that fans out to all hooks. Nil hooks are filtered. If only one non-nil hook remains, it's returned directly (no wrapper overhead).

### Integration with Runtime

```go
rt := niro.NewRuntime(llm).WithHook(hook)
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
- `[]byte` for binary data — pooled via `BytePool` (zero alloc) or single allocation per chunk
- `*Usage` and `*ResponseMeta` — pooled via `sync.Pool` (zero alloc, ~21-27ns)
- These paths are inherently I/O bound, but pooling eliminates GC pressure at scale

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

## Structured Output (JSON Schema)

Niro supports schema-constrained output with typed decoding inspired by Genkit’s
GenerateData/GenerateDataStream model. The request sets:

- `ResponseFormat = "json_schema"`
- `ResponseSchema = <JSON Schema bytes>`

Helpers in [structured.go](structured.go) provide two paths:

1. **Final typed output** — `GenerateStructured[T]` runs the request and
   unmarshals the final JSON into `T`.
2. **Partial + final streaming** — `StreamStructured[T]` parses partial JSON as
   soon as the stream becomes valid, then emits the final typed value when the
   stream ends.

Decoding uses the configurable JSON backend defined in [json.go](json.go) and
performs a fast `JSONValid` check before attempting to unmarshal partial data.

## Package Structure

Niro uses a **multi-module** layout. The core module (`github.com/alexedtionweb/niro-stream`) has **zero external dependencies**. Each SDK provider is a separate Go module with its own `go.mod` — users only pull the SDKs they need.

```text
github.com/alexedtionweb/niro-stream                          ← root module (zero external deps)
├── go.mod                           module github.com/alexedtionweb/niro-stream
├── go.work                          workspace linking all sub-modules (dev only)
├── doc.go                           Package documentation
├── frame.go                         Frame, Kind, Signal, ToolCall, ToolResult, Tool, Usage
├── message.go                       Message, Part, Role — conversation model
├── stream.go                        Stream, Emitter, NewStream — the core pipe
├── processor.go                     Processor interface, Map/Filter/Tap/TextOnly/Accumulate
├── pipeline.go                      Pipeline — concurrent goroutine-per-stage chain
├── provider.go                      Provider interface, Request, Options, Extra
├── hook.go                          Hook interface, GenerateStartInfo/EndInfo, NoOpHook, Hooks()
├── orchestrate.go                   Fan, Race, Sequence — concurrent workflow primitives
├── runtime.go                       Runtime — lifecycle composer (Provider + Pipeline + Hook)
├── pool.go                          BytePool, pooled frame constructors, Usage/ResponseMeta pools
├── transport.go                     Transport(), HTTPClient(), DefaultTransport, DefaultHTTPClient
├── cache.go                         Cache — sharded LRU response cache with TTL
├── registry.go                      Registry — named provider routing
├── components.go                    Component interface, ComponentHost — plugin lifecycle
├── multitenancy.go                  MultiTenantProvider — client-scoped provider routing
├── json.go                          Configurable JSON backend (Fiber-compatible list)
├── structured.go                    Structured output helpers (JSON Schema → typed)
├── errors.go                        Error type, ErrorCode, semantic error checkers, error helpers
├── validation.go                    Request.Validate(), Message.Validate(), Tool.Validate(), ToolCall.Validate()
├── retry.go                         RetryProvider, BackoffStrategy, smart retry hints
├── cost.go                          Cost tracking, ModelPricing, PricingRegistry, pricing for known models
├── timeout.go                       Timeout enforcement, request tracing, RequestID generation, TraceContext
├── input_stream.go                  Native + fallback input streaming adapter
├── tools.go                         Tool execution loop, automatic tool calling, ToolExecutor interface
├── tooling.go                       ToolDefinition, Toolset, schema validator, runtime hooks, ToolingProvider
├── bench_test.go                    Benchmarks for core primitives + infrastructure
├── helpers_test.go                  Shared test helpers, mocks, assertions
├── frame_test.go                    Tests for Frame, Kind, Signal, Usage, ToolChoice
├── message_test.go                  Tests for Message constructors
├── stream_test.go                   Tests for Stream, Emitter, CollectText, Forward
├── processor_test.go                Tests for Map, Filter, Tap, TextOnly, Accumulate
├── pipeline_test.go                 Tests for Pipeline stages
├── provider_test.go                 Tests for ProviderFunc, Request.EffectiveMessages
├── hook_test.go                     Tests for NoOpHook, Hooks() composition
├── orchestrate_test.go              Tests for Fan, Race, Sequence
├── runtime_test.go                  Tests for Runtime lifecycle
├── pool_test.go                     Tests for BytePool, pooled constructors, Usage pool
├── cache_test.go                    Tests for Cache hit/miss, TTL, LRU, concurrency
├── registry_test.go                 Tests for Registry CRUD, Generate, concurrency
├── components_test.go               Tests for ComponentHost lifecycle
├── multitenancy_test.go             Tests for MultiTenantProvider routing
├── transport_test.go                Tests for Transport, HTTPClient defaults
├── json_test.go                     Tests for configurable JSON backend
├── structured_test.go               Tests for GenerateStructured, StreamStructured
├── errors_test.go                   Tests for Error type, checkers, wrapping
├── validation_test.go               Tests for Request/Message/Tool validation
├── retry_test.go                    Tests for RetryProvider, backoff strategies
├── cost_test.go                     Tests for cost calculation, PricingRegistry
├── tracing_test.go                  Tests for RequestID, TraceContext, TracingProvider
├── tools_test.go                    Tests for ToolLoop, Toolset, ToolingProvider
├── input_stream_test.go             Tests for input streaming (native + fallback)
│
├── internal/
│   └── sse/
│       ├── reader.go                SSE event reader (stdlib-only)
│       └── reader_test.go
│
├── provider/
│   ├── compat/                      OpenAI-compatible HTTP+SSE (in root module, stdlib-only)
│   │   ├── compat.go
│   │   └── compat_test.go
│   ├── openai/                      separate module: github.com/alexedtionweb/niro-stream/provider/openai
│   ├── anthropic/                   separate module: github.com/alexedtionweb/niro-stream/provider/anthropic
│   ├── google/                      separate module: github.com/alexedtionweb/niro-stream/provider/google
│   └── bedrock/                     separate module: github.com/alexedtionweb/niro-stream/provider/bedrock
│
├── plugin/
│   └── agent/                       separate module: github.com/alexedtionweb/niro-stream/plugin/agent
│       ├── agent.go                 Declarative agent with memory + routing
│       ├── orchestrator.go          Step-based orchestrator (tool, llm, condition, etc.)
│       ├── components.go            Agent-specific components
│       └── decl.go                  YAML/JSON agent declaration types
│
└── examples/                        separate module: github.com/alexedtionweb/niro-stream/examples
    ├── chat/main.go
    ├── tools/main.go
    ├── parallel/main.go
    └── pipeline/main.go
```

### Multi-Module Design

**Why separate modules?**

Without separate modules, `go get github.com/alexedtionweb/niro-stream` would pull every SDK (OpenAI, Anthropic, Google, AWS) into the dependency graph — even if you only use one provider. This balloons `go.sum`, slows builds, and introduces transitive dependencies you don't control.

With the plugin model:

```bash
# Core — zero external deps
go get github.com/alexedtionweb/niro-stream

# Only the provider you need
go get github.com/alexedtionweb/niro-stream/provider/openai
```

Your binary contains only the SDK you actually import. The compat provider (stdlib HTTP+SSE) stays in the root module since it has zero external deps.

**Development workflow**: A `go.work` file links all sub-modules for local development. Each provider `go.mod` has a `replace` directive pointing to the local root. These are removed before tagging releases.

### SDK Extensibility Pattern

Every SDK provider exposes three extension points:

1. **`Client()`** — returns the underlying SDK client for direct API access
2. **`WithRequestHook(fn)`** — provider-level option: hook runs on every request
3. **`Request.Extra`** — per-request hook via `niro.Request.Extra` field

```go
// Provider-level: every request gets logprobs
llm := openai.New(key, openai.WithRequestHook(func(p *oai.ChatCompletionNewParams) {
    p.LogProbs = oai.Bool(true)
}))

// Per-request: this one request only
stream, _ := llm.Generate(ctx, &niro.Request{
    Messages: msgs,
    Extra: openai.RequestHook(func(p *oai.ChatCompletionNewParams) {
        p.TopLogProbs = oai.Int(5)
    }),
})
```

The `RequestHook` type is provider-specific — it receives the raw SDK parameter struct. This gives full SDK access without forking.

### Dependency Graph

```
                github.com/alexedtionweb/niro-stream (core)         ← stdlib only, zero deps
              /    |       \        \
         stream  pipeline  hook  orchestrate
                       \
                    provider.go  ←──  Request.Extra any (hooks into SDKs)
                   /    |    |    \         \
            openai anthropic google bedrock  compat
            (own   (own      (own   (own    (root module,
            module) module)  module) module)  stdlib-only)
```

## Production Infrastructure

Niro includes purpose-built infrastructure for high-concurrency production deployments (millions of concurrent LLM calls).

### BytePool — Zero-Allocation Media Buffers

**Problem**: Under millions of concurrent calls, per-frame `[]byte` allocations for audio/image/video data create GC pressure that dominates latency.

**Solution**: `BytePool` is a size-class `sync.Pool` with three buckets:

```
┌──────────────────────────────────────────────┐
│                BytePool                       │
├──────────┬──────────┬────────────────────────┤
│ small    │ ≤4 KB    │ Audio chunks (20ms PCM) │
│ medium   │ ≤64 KB   │ Larger audio, small imgs│
│ large    │ ≤1 MB    │ Images, video frames    │
│ (direct) │ >1 MB    │ Allocated directly      │
└──────────┴──────────┴────────────────────────┘
```

**Key design choices**:

- Pointers stored in pools (avoids interface boxing allocations)
- Buffers returned via `Put()` are reset to `[:0]` — no stale data leaks
- Oversized buffers (>2× large class) are dropped to prevent pool bloat
- `DefaultBytePool` is process-wide; providers can create isolated pools

**Pooled frame constructors** (`AudioFramePooled`, `ImageFramePooled`, `VideoFramePooled`) copy data into a pooled buffer and return a Frame. The consumer calls `pool.Put(frame.Data)` when done.

### Transport — HTTP Connection Pooling

**Problem**: Each LLM API call over HTTPS incurs TCP+TLS handshake cost. At scale, connection establishment becomes a bottleneck.

**Solution**: `Transport()` creates an `*http.Transport` optimized for LLM traffic:

- **Keep-alive**: 30s interval, 120s idle timeout — amortizes handshakes
- **Connection pool**: GOMAXPROCS×64 total idle, GOMAXPROCS×16 per host
- **TLS**: Session ticket resumption, TLS 1.2+ minimum
- **HTTP/2**: Negotiated via ALPN (all major LLM APIs support it)
- **Buffers**: 64KB write (typical request payloads), 32KB read (SSE streams)

`DefaultTransport` and `DefaultHTTPClient` are process-wide singletons. The compat provider uses `DefaultHTTPClient` by default.

### Cache — Sharded LRU Response Cache

**Problem**: Identical requests (same model + messages + tools + options) should not hit the provider repeatedly.

**Solution**: `Cache` provides a sharded (64 shards), TTL-aware LRU cache:

```
Request → SHA-256(model + messages + tools + options) → shard[hash % 64]
    │
    ├── Hit: replay cached frames as a new Stream
    └── Miss: tee the provider's stream → emit to consumer + store in cache
```

**Key design choices**:

- **64 shards** — power of 2 for fast modulo, eliminates lock contention
- **Per-shard RWMutex** — reads are write-locked only for LRU promotion (O(1))
- **Intrusive doubly-linked list** — O(1) LRU promote/evict, no heap allocation for list nodes
- **SHA-256 keying** — deterministic, collision-resistant, includes all request fields
- **Tee pattern** — on cache miss, frames are streamed to consumer AND cached simultaneously
- **Atomic hit/miss counters** — lock-free stats via `atomic.Int64`

Cache wraps any `Provider` transparently via `cache.Wrap(provider)`.

### Registry — Named Provider Routing

**Problem**: Production deployments run multiple providers (OpenAI, Anthropic, cached variants, fallbacks) and need to route by name at request time.

**Solution**: `Registry` is a `sync.RWMutex`-protected `map[string]Provider`:

```go
reg := niro.NewRegistry()
reg.Register("openai", openaiProvider)
reg.Register("cached-openai", cache.Wrap(openaiProvider))

// Request-time routing:
stream, err := reg.Generate(ctx, "openai", req)
```

Reads use `RLock` — zero contention under concurrent lookups. Writes use full lock. `All()` returns a snapshot (copy of map).

### Object Pools

`sync.Pool`-backed pools for frequently allocated hot-path objects:

| Pool                | Object          | Allocs | Cost  |
| ------------------- | --------------- | ------ | ----- |
| `GetUsage()`        | `*Usage`        | 0      | ~21ns |
| `GetResponseMeta()` | `*ResponseMeta` | 0      | ~27ns |

Objects are zeroed on `Get` and cleaned on `Put` (maps set to nil to prevent retention).

## Error Handling & Validation

### Semantic Error Types (`errors.go`)

Errors use typed error codes for proper handling:

```go
type ErrorCode int

const (
    ErrCodeInvalidRequest = 400 + iota
    ErrCodeAuthenticationFailed           // don't retry
    ErrCodeModelNotFound                  // don't retry
    ErrCodeRateLimited = 429             // retry with backoff
    ErrCodeProviderError = 500 + iota
    ErrCodeServiceUnavailable             // retry
    ErrCodeTimeout                        // retry with longer deadline
)

type Error struct {
    Code       ErrorCode
    Message    string
    Err        error           // underlying error (chaining)
    Provider   string
    RequestID  string          // trace ID
    Retryable  bool
    StatusCode int
}

// Helpers: IsRetryable(err), IsRateLimited(err), IsAuthError(err), IsTimeout(err)
```

### Request Validation (`validation.go`)

Request.Validate() checks: non-empty messages, valid ResponseFormat, JSON Schema validity, option bounds (Temperature ∈ [0, 2.0], TopP ∈ [0, 1.0], etc.), tool definitions.

## Retry & Backoff (`retry.go`)

**BackoffStrategy** implementations:

- ConstantBackoff: Fixed delay
- ExponentialBackoff: Exponential growth with optional jitter

**RetryProvider** wraps Provider with configurable retry logic. Only retries errors marked retryable. Respects context cancellation.

**Smart retry path**: when a provider exposes retry capability hints, `WrapWithSmartRetry` can skip outer retries if SDK/client retries are already enabled, preventing redundant retry multiplication.

## Cost Tracking (`cost.go`)

**ModelPricing** + **PricingRegistry** for per-model cost tracking. Default pricing for OpenAI, Anthropic, Google, AWS Bedrock (2025).

## Timeouts & Tracing (`timeout.go`)

**TimeoutProvider**: Generation timeout enforcement.

**TraceContext**: Request-scoped trace data (RequestID, UserID, SessionID). Used by hooks for logging, cost tracking, distributed tracing.

## Tool Execution (`tools.go`)

**ToolExecutor**: Execute tool calls.

**ToolLoop**: Multi-turn tool calling (Generate → Execute → Generate → ... until no tools or max rounds).

Tool results are fed back into the same request as `ToolResult` parts before the next model turn.

## Tool Abstraction Layer (`tooling.go`)

`ToolDefinition` + `Toolset` provide a higher-level, Genkit-like authoring model:

- Define tools from raw JSON Schema
- Validate tool args before execution
- Register runtime hooks for validation and execution lifecycle
- Convert definitions to `Request.Tools` automatically
- Wrap a base provider with `ToolingProvider` for integrated tool execution loop

## Input Streaming (`input_stream.go`)

`GenerateInputStream` supports:

- Native provider input streaming (if implemented)
- Fallback mode that aggregates input stream into a standard request and runs regular generation

## Future Extension Points

The architecture is designed for forward compatibility:

### Audio Streaming (STT → LLM → TTS)

```
AudioFrame → [STT Processor] → TextFrame → [Provider] → TextFrame → [TTS Processor] → AudioFrame
```

The Pipeline already supports this. Frame already has `KindAudio` and `Mime`.

### Tool Execution Graphs

Current implementation supports iterative tool loops with execution + feedback.
Future work can add DAG-style dependency scheduling and speculative parallel branches across tool nodes.

### Realtime Agents

Current: single-turn generate, manual tool loop.
Future: `Agent` that manages multi-turn state, automatic tool execution, and continuous streaming with interruption support.

### Provider Middleware

Current: Hook provides observability, `RequestHook` enables SDK-level customization, `Cache` provides response caching, `Registry` enables named routing.
Future: Provider middleware for retries, rate limiting, fallback chains, circuit breaking.

### Community Providers

The plugin model makes it easy to add providers without touching the core:

```go
// Third-party provider — just implement niro.Provider
// and publish as github.com/alexedtionweb/niro-stream/provider/mistral (or your own module path)
```
