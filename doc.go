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
//   - [Processor]: Composable stream transformer (the building block)
//   - [Pipeline]: Concurrent chain of Processors with automatic lifecycle
//   - [Provider]: LLM backend interface (OpenAI, Anthropic, Google, Bedrock, or custom)
//   - [Hook]: Telemetry / observability interface for tracing every generation
//
// # Quick Start
//
//	provider := openai.New(os.Getenv("OPENAI_API_KEY"))
//
//	stream, err := provider.Generate(ctx, &niro.Request{
//	    Model: "gpt-4o",
//	    Messages: []niro.Message{
//	        niro.UserText("Hello!"),
//	    },
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	for stream.Next(ctx) {
//	    fmt.Print(stream.Frame().Text)
//	}
//	usage := stream.Usage()
//	fmt.Printf("tokens: %d in, %d out\n", usage.InputTokens, usage.OutputTokens)
//
// # Design Principles
//
// Streaming-first, not streaming-compatible. Minimal abstractions, maximum
// control. Zero magic. Composable pipelines. Backpressure-aware.
// Low allocations. Go idiomatic. Production-first.
package niro
