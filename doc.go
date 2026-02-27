// Package ryn is a streaming-first LLM runtime for Go.
//
// Ryn provides a minimal, composable architecture for building real-time
// AI systems. It is designed for low-latency, multimodal streaming
// pipelines — not request/response wrappers.
//
// # Core Concepts
//
//   - [Frame]: Universal unit of data (text tokens, audio, image, video, tool calls)
//   - [Stream]: Backpressure-aware, cancellable sequence of Frames
//   - [Processor]: Composable stream transformer (the building block)
//   - [Pipeline]: Concurrent chain of Processors with automatic lifecycle
//   - [Provider]: LLM backend interface — bring OpenAI, Anthropic, Ollama, or your own
//
// # Quick Start
//
//	provider := openai.New(os.Getenv("OPENAI_API_KEY"))
//
//	stream, err := provider.Generate(ctx, &ryn.Request{
//	    Model: "gpt-4o",
//	    Messages: []ryn.Message{
//	        ryn.Text(ryn.RoleUser, "Hello!"),
//	    },
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	for stream.Next(ctx) {
//	    fmt.Print(stream.Frame().Text)
//	}
//
// # Design Principles
//
// Streaming-first, not streaming-compatible. Minimal abstractions, maximum
// control. Zero magic. Composable pipelines. Backpressure-aware.
// Low allocations. Go idiomatic. Production-first.
package ryn
