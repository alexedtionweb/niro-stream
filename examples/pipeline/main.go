// Command pipeline demonstrates post-processing pipelines with hooks.
//
// Usage:
//
// OPENAI_API_KEY=sk-... go run ./examples/pipeline
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"ryn.dev/ryn"
	"ryn.dev/ryn/hook"
	"ryn.dev/ryn/pipe"
	"ryn.dev/ryn/provider/openai"
	"ryn.dev/ryn/runtime"
)

func main() {
	ctx := context.Background()

	llm := openai.New(os.Getenv("OPENAI_API_KEY"))

	// Build a processing pipeline
	pipeline := pipe.New(
		// Stage 1: filter to text only
		pipe.TextOnly(),
		// Stage 2: transform tokens
		pipe.Map(func(f ryn.Frame) ryn.Frame {
			f.Text = strings.ToUpper(f.Text)
			return f
		}),
	).WithBuffer(32)

	// Create a logging hook
	h := &logHook{}

	// Wire everything together via Runtime
	rt := runtime.New(llm).
		WithPipeline(pipeline).
		WithHook(h)

	stream, err := rt.Generate(ctx, &ryn.Request{
		Model:        "gpt-4o",
		SystemPrompt: "You are a pirate. Be concise.",
		Messages:     []ryn.Message{ryn.UserText("Tell me about Go channels.")},
		Options:      ryn.Options{MaxTokens: 128, Temperature: ryn.Temp(0.9)},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate: %v\n", err)
		os.Exit(1)
	}

	for stream.Next(ctx) {
		fmt.Print(stream.Frame().Text)
	}
	fmt.Println()

	if err := stream.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "stream: %v\n", err)
	}
}

// logHook logs telemetry events to stderr.
type logHook struct {
	hook.NoOpHook
}

func (h *logHook) OnGenerateStart(ctx context.Context, info hook.GenerateStartInfo) context.Context {
	fmt.Fprintf(os.Stderr, "[hook] generate start: model=%s messages=%d tools=%d\n",
		info.Model, info.Messages, info.Tools)
	return ctx
}

func (h *logHook) OnGenerateEnd(ctx context.Context, info hook.GenerateEndInfo) {
	fmt.Fprintf(os.Stderr, "[hook] generate end: model=%s duration=%s tokens=%d finish=%s\n",
		info.Model, info.Duration.Round(time.Millisecond), info.Usage.TotalTokens, info.FinishReason)
}

func (h *logHook) OnError(ctx context.Context, err error) {
	fmt.Fprintf(os.Stderr, "[hook] error: %v\n", err)
}
