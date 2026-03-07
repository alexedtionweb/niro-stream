# API Reference Overview

Quick reference to packages and main types for integration and navigation. For full Go docs see [pkg.go.dev](https://pkg.go.dev/github.com/alexedtionweb/niro-stream).

---

## Root package: `github.com/alexedtionweb/niro-stream`

| Symbol | Description |
|--------|-------------|
| **Provider** | Interface: `Generate(ctx, *Request) (*Stream, error)`. Implement or use provider packages. |
| **ProviderFunc** | Function adapter for Provider. |
| **Request** | Model, SystemPrompt, Messages, Tools, ToolChoice, ResponseFormat, ResponseSchema, Options, Extra. |
| **Request.Validate()** | Validates request before calling provider. |
| **Request.EffectiveMessages()** | Messages with SystemPrompt prepended as system message. |
| **Options** | MaxTokens, Temperature, TopP, TopK, Stop, ThinkingBudget, Cache, ExperimentalReasoning. |
| **Temp(v)** | Returns *float64 for Options.Temperature. |
| **Stream** | Read side: Next(ctx), Frame(), Err(), Usage(), Response(), Chan(). |
| **Emitter** | Write side: Emit(ctx, Frame), Close(), Error(err), SetResponse(meta). |
| **NewStream(buf)** | Returns (stream, emitter). |
| **Frame** | Kind, Text, Data, Mime, Tool, Result, Usage, Custom, Signal. |
| **Kind** | KindText, KindAudio, KindToolCall, KindToolResult, KindUsage, KindCustom, … |
| **TextFrame(s)**, **UsageFrame(u)**, **ToolCallFrame(t)** | Frame constructors. |
| **Message** | Role, Content (parts). UserText, AssistantText, SystemText, Multi, ToolMessage. |
| **Tool** | Name, Description, Parameters (JSON Schema). |
| **ToolChoice** | ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired, ToolChoiceFunc(name). |
| **Usage** | InputTokens, OutputTokens, TotalTokens, Detail map. |
| **ResponseMeta** | Model, FinishReason, ID, Usage, ProviderMeta. |
| **FinishReasonStop**, **FinishReasonContentFilter**, **FinishReasonLength**, … | Well-known finish reasons. |
| **IsRefusalOrBlock(reason)** | True for content_filter or other (empty/blocked response). |
| **Error** | Code, Message, Err, Provider, RequestID, Retryable, StatusCode. |
| **IsRetryable(err)**, **IsRateLimited(err)**, **IsAuthError(err)**, **IsTimeout(err)** | Error classifiers. |
| **NewError**, **WrapError** | Construct typed errors. |
| **CollectText(ctx, stream)** | Consumes stream, returns concatenated text. |
| **Forward(ctx, src, dst)** | Forwards all frames from src to emitter dst. |
| **DefaultStreamBuffer** | Default channel buffer size (32). |
| **Pipe(procs...)** | Builds pipeline (pipe package). |
| **ComposedProvider(base, timeout, retryConfig)** | Composes timeout + retry (middleware). |
| **Cache**, **NewCache**, **Registry**, **MultiTenantProvider** | Caching and routing (see subpackages). |
| **BytePool**, **DefaultBytePool** | Buffer pooling. |
| **Transport()**, **DefaultHTTPClient** | HTTP transport. |
| **GenerateStructured**, **StreamStructured** | In structured package. |

---

## Package: `runtime`

| Symbol | Description |
|--------|-------------|
| **Runtime** | Composes Provider + optional Pipeline + Hook. |
| **New(provider)** | Creates Runtime. |
| **WithHook**, **WithPipeline**, **WithCacheEngine**, **WithPrefixNormalizer** | Options. |
| **Generate(ctx, req)** | Validates req, runs provider, wraps stream with hook and pipeline. |

---

## Package: `hook`

| Symbol | Description |
|--------|-------------|
| **Hook** | OnGenerateStart, OnGenerateEnd, OnFrame, OnToolCall, OnToolResult, OnError. |
| **NoOpHook** | Embed to implement only some methods. |
| **Compose(hooks...)** | Combines multiple hooks. |
| **GenerateStartInfo**, **GenerateEndInfo** | Metadata for start/end. |
| **WrapStream** | Wraps a stream to invoke hook on each frame. |

---

## Package: `pipe`

| Symbol | Description |
|--------|-------------|
| **Processor** | Process(ctx, in *Stream, out *Emitter) error. |
| **Pipeline** | Chain of processors; Run(ctx, in) returns output stream. |
| **New(procs...)**, **WithBuffer(n)** | Build pipeline. |
| **Map(fn)**, **Filter(fn)**, **Tap(fn)** | Frame processors. |
| **TextOnly()**, **PassThrough()**, **Accumulate()** | Built-in processors. |

---

## Package: `orchestrate`

| Symbol | Description |
|--------|-------------|
| **Fan(ctx, fns...)** | N streams in parallel; merged into one stream. |
| **Race(ctx, fns...)** | N streams; returns (text, usage, err) of first to complete; cancels others. |
| **Sequence(ctx, fns...)** | Chained: each fn gets previous text; returns last stream. |

---

## Package: `tools`

| Symbol | Description |
|--------|-------------|
| **ToolExecutor** | Execute(ctx, name, args) (string, error). |
| **ToolExecutorFunc** | Function adapter. |
| **ToolLoop** | Multi-round tool loop. |
| **NewToolLoop(executor, maxRounds)** | Creates loop. |
| **GenerateWithTools(ctx, provider, req)** | Runs loop; returns stream with final text. |
| **ToolStreamOptions** | MaxRounds, ToolTimeout, StreamBuffer, Approver. |
| **DefaultToolStreamOptions()** | Sensible defaults. |
| **Toolset** | Register tools with schema and handlers. |
| **ToolingProvider** | Wraps provider with toolset; automatic tool execution. |
| **HandoffSignal** | Return from tool handler to signal handoff (DSL). |

---

## Package: `output`

| Symbol | Description |
|--------|-------------|
| **Sink** | OnAgentStart, OnText, OnThinking, OnToolCall, OnToolResult, OnCustom, OnEnd. |
| **Route(ctx, stream, sink)** | Tees stream to sink; returns new stream. |
| **RouteAgent(ctx, stream, sink, agent)** | Like Route; injects agent in context and calls OnAgentStart. |
| **AgentFromContext(ctx)** | Returns agent name from context. |
| **ContextWithAgent(ctx, agent)** | Puts agent in context. |

---

## Package: `middleware`

| Symbol | Description |
|--------|-------------|
| **NewRetryProvider(p, config)** | Retries on retryable errors. |
| **RetryConfig** | MaxAttempts, Backoff, ShouldRetry, OnRetry. |
| **ExponentialBackoff** | InitialDelay, Multiplier, MaxDelay, Jitter. |
| **NewTimeoutProvider(p, timeout)** | Enforces generation timeout. |
| **WrapCache(p, cache, opts)** | Caches responses. |
| **TracingProvider** | Injects trace context. |

---

## Package: `registry`

| Symbol | Description |
|--------|-------------|
| **Registry** | Register(name, provider), Generate(ctx, name, req), Names(). |
| **New()** | Creates registry. |
| **MultiTenantProvider** | Routes by Request.Client or selector. |
| **WithDefaultClient**, **WithClientSelector**, **WithClientMutator** | Options. |

---

## Package: `structured`

| Symbol | Description |
|--------|-------------|
| **GenerateStructured[T](ctx, p, req, schema)** | One shot; returns (result, resp, usage, err). |
| **StreamStructured[T](ctx, p, req, schema)** | Streaming; partial and final events. |

---

## Package: `plugin/agent`

| Symbol | Description |
|--------|-------------|
| **Agent** | Runtime with memory, components, peers. |
| **New(provider, opts)** | Creates agent. |
| **WithMemory**, **WithComponent**, **WithPeer** | Options. |
| **Memory** | Load(sessionID), Save(sessionID, messages). |
| **Run**, **RunStream** | Execute turn. |
| **Orchestrator** | Runs declarative agent definitions. |

---

## Package: `plugin/dsl`

| Symbol | Description |
|--------|-------------|
| **ParseDSL**, **ParseDSLFile** | Parse agents JSON. |
| **Validate**, **Compile** | Validate and compile to NiroDefinition. |
| **ParseWorkflow**, **CompileWorkflow** | Parse and compile workflow JSON. |
| **Runner** | NewRunner(nd, provider, opts). Stream(ctx, runCtx, agentName, sessionID). |
| **WithOutputSink**, **WithToolOptions** | Runner options. |
| **RunContext** | Get/Set for template and expr. |
| **NewRunContext** | Creates context. |
| **CompiledWorkflow** | Run(ctx, runCtx, sessionID); Fan, Race, Sequence, FanThen. |

---

## Provider packages

- **provider/openai**: `New(apiKey)`, `Client()`, `WithRequestHook`, `RequestHook` in Request.Extra.
- **provider/anthropic**: Same pattern.
- **provider/google**: `New(apiKey)`, `WithModel`, `WithClientOption`, `ThinkingConfig` via Options.ThinkingBudget.
- **provider/bedrock**: `New(awsConfig)`, `WithInferenceProfile`, Extras for per-request override.
- **provider/compat**: `New(baseURL, apiKey)` for OpenAI-compatible HTTP/SSE endpoints.

All implement `niro.Provider` and return streams that emit Frame (text, tool call, usage, etc.) and set ResponseMeta before close.
