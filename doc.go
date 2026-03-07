// Package niro is a streaming-first LLM runtime for Go.
//
// Niro provides a minimal, composable architecture for building real-time
// AI systems. It is designed for low-latency, multimodal streaming
// pipelines — not request/response wrappers.
//
// # Core Concepts
//
//   - [Frame]: Universal unit of data (text tokens, audio, image, video, tool calls)
//   - [Stream]: Backpressure-aware, cancellable sequence of Frames with usage tracking
//   - [Processor]: Composable stream transformer (building block in pipe package)
//   - [Pipeline]: Concurrent chain of Processors (pipe package)
//   - [Provider]: LLM backend interface (OpenAI, Anthropic, Google, Bedrock, or custom)
//   - [Hook]: Telemetry interface for tracing every generation (hook package)
//
// # Quick Start
//
//	provider := openai.New(os.Getenv("OPENAI_API_KEY"))
//
//	stream, err := provider.Generate(ctx, &niro.Request{
//	    Model: "gpt-4o",
//	    Messages: []niro.Message{niro.UserText("Hello!")},
//	})
//	if err != nil { log.Fatal(err) }
//
//	for stream.Next(ctx) {
//	    fmt.Print(stream.Frame().Text)
//	}
//	usage := stream.Usage()
//	fmt.Printf("tokens: %d in, %d out\n", usage.InputTokens, usage.OutputTokens)
//
// # Subpackages
//
//   - runtime: Compose Provider with Hook and Pipeline
//   - hook: Observability (OnGenerateStart, OnFrame, OnGenerateEnd)
//   - pipe: Processors and Pipeline for stream transformation
//   - orchestrate: Fan (parallel), Race (first wins), Sequence (chained)
//   - tools: ToolLoop, Toolset, ToolingProvider for automatic tool execution
//   - output: Route / RouteAgent to tee streams into Sink callbacks
//   - middleware: Retry, Timeout, Cache, Tracing wrappers
//   - registry: Named provider registration and MultiTenantProvider
//   - structured: GenerateStructured, StreamStructured for JSON schema output
//   - plugin/agent: Optional agent runtime with memory and components
//   - plugin/dsl: JSON-defined agents and workflows with handoff and fan_then
//
// # Integration
//
// For a full guide to integrating Niro into another project (dependencies,
// validation, errors, hooks, tools, middleware, testing), see the docs
// directory in the repo: docs/INTEGRATION.md. API overview: docs/API_REFERENCE.md.
//
// # Design Principles
//
// Streaming-first. Minimal abstractions, maximum control. Zero magic.
// Composable pipelines. Backpressure-aware. Low allocations. Go idiomatic.
package niro
