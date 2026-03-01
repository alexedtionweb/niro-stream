# Niro — Capabilities Overview

This document summarizes the capabilities implemented in this codebase. Each entry has a short title and a concise description of the feature and its intent.

- **Streaming-First Runtime**: Core primitives (`Frame`, `Stream`, `Emitter`) expose token-level and multimodal frames via a backpressure-aware stream. Producers emit frames as data arrives; consumers read incrementally.

- **Experimental Extension Frames**: `KindCustom` with `ExperimentalFrame{Type, Data}` allows provider-specific payloads (for example reasoning summaries/traces) without expanding core frame variants.

- **Provider Adapter Interface**: The `Provider` interface abstracts SDKs and endpoints (OpenAI, Anthropic, Google, AWS Bedrock, compat HTTP). Providers implement `Generate(ctx, *Request) (*Stream, error)` and expose SDK hooks/clients.

- **SDK-backed Providers**: Dedicated provider modules for OpenAI, Anthropic, Google Gemini, and AWS Bedrock. Each provider exposes the underlying SDK client and provider-level/per-request hooks for raw SDK param customization.

- **Tooling & Toolset Abstraction**: High-level Genkit-like tooling (`ToolDefinition`, `Toolset`) with JSON Schema-based parameter definitions, validation, runtime hooks, and `ToolingProvider` that integrates declared tools into the generation loop.

- **Automatic Tool Execution Loop**: `ToolLoop` and `StreamWithToolHandling` execute multi-round tool calling (Generate → Execute → Generate ...), preserve streaming, and feed `ToolResult` frames back into subsequent turns.

- **Tool Schema Validation**: Configurable `ToolSchemaValidator` (global and per-Toolset) validates tool call payloads against JSON Schema before execution.

- **Input Streaming Adapter**: `input_stream.go` provides native input streaming integration when supported by a provider, with a robust fallback path that aggregates streamed input into a standard request.

- **Smart Retry & Backoff**: `retry.go` provides retry/backoff primitives and a smart wrapper (`WrapWithSmartRetry`) that avoids redundant outer retries when providers or SDKs already perform retries.

- **Timeouts & Tracing**: `timeout.go` enforces generation timeouts and propagates trace context (RequestID, user/session IDs) to hooks for observability.

- **Usage-First Billing Inputs**: Core runtime emits raw usage (`Usage` + `Usage.Detail`) for external billing systems; pricing is handled outside the runtime.

- **Token Budget Controls**: `Options.MaxTokens` sets output token caps per request; providers map this to native SDK limits and normalized usage is returned via `stream.Usage()` / `ResponseMeta.Usage`.

- **Provider-Agnostic Prompt Cache Hints**: `Options.Cache` expresses cache intent (`Auto|Prefer|Require|Bypass`) without leaking provider types; runtime derives tenant-safe deterministic keys and passes hints through context only when enabled.

- **Cache Extension Hooks**: Runtime exposes `WithPrefixNormalizer(...)` for deterministic prefix canonicalization and `WithCacheEngine(...)` for pluggable local prefix lookup/store integration.

- **Reasoning Metadata Keys**: Stable keys (`UsageReasoningTokens`, `UsageReasoningCost`) are available for provider-reported reasoning usage in `Usage.Detail`.

- **Cache — Sharded LRU**: `cache.go` implements a sharded LRU response cache with TTL and a tee pattern to replay frames on cache hits while storing on misses.

- **Registry — Named Provider Routing**: `Registry` allows registering multiple providers by name and routing requests at runtime. `DefaultRegistry` is available as a process-global option.

- **Multi-Tenancy Router**: `MultiTenantProvider` adds runtime selection of provider/client per-request via `Request.Client`, context (`WithClient`), or a custom selector; supports per-client request mutators for tenant-specific auth/config.

- **Transport Optimizations**: `transport.go` provides tuned `*http.Transport` defaults optimized for streaming LLM workloads (keepalive, pool sizing, TLS, HTTP/2 tuning).

- **BytePool & Low-allocation Primitives**: `pool.go` provides size-class `sync.Pool` buffer pooling and pooled frame constructors to minimize GC pressure under high concurrency.

- **Structured Output (JSON Schema)**: `structured.go` supports typed decode and streaming parse of JSON Schema constrained model outputs with `GenerateStructured` and `StreamStructured` helpers.

- **Configurable JSON Backend**: `json.go` allows swapping JSON marshal/unmarshal/validation implementations (stdlib or high-performance backends) globally.

- **Experimental Reasoning Gate**: `Options.ExperimentalReasoning` is an opt-in flag for providers that support reasoning extensions; unsupported providers return explicit errors.

- **Hook Interface (Observability)**: `hook.go` defines hooks (`OnGenerateStart`, `OnFrame`, `OnGenerateEnd`, `OnError`) to instrument generation lifecycle and integrate telemetry, logging, or tracing.

- **Pipeline & Processor Primitives**: `Processor` and `Pipeline` primitives (Map, Filter, Tap, Fan, Race, Sequence) allow composing concurrent, backpressure-respecting stream transformations.

- **Orchestration Primitives**: High-level concurrency patterns (Fan, Race, Sequence) for parallel/multi-model orchestration, speculative execution, and hedging.

- **Tooling Provider Bridge**: `tooling.go` includes `ToolingProvider` that converts a `Toolset` into provider `Request.Tools`, runs the tool loop, and emits lifecycle hooks.

- **Component Host & Plugin Model**: `components.go` introduces `Component` and `ComponentHost` (register/start/close) for optional runtime plugins. This enables modular agent plugins without coupling to core runtime.

- **Agent Plugin (Optional Module)**: `/plugin/agent` implements an optional agent runtime layer (memory interfaces, peer calls, in-memory memory adapter, agent components like `ToolingComponent` and `MultiTenantComponent`) that mounts atop the core runtime as a plugin.

- **Tool Execution Hooks & Telemetry**: Tool lifecycle hooks (`OnToolValidate`, `OnToolExecuteStart`, `OnToolExecuteEnd`) are available for auditing, instrumentation, and security checks.

- **Test Coverage & Examples**: Test suite exercises registry, provider adapters, tool loop, toolset behaviors, multi-tenancy, and component host lifecycle. Example programs under `examples` demonstrate chat, tools, parallel, and pipeline usages.

- **Design Goals & Production Focus**: The codebase targets production systems: low-latency streaming, low allocations, pluggable providers, observability, multi-tenancy, and safe retry/timeout semantics.
