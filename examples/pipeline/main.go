// Pipeline example — LLM output through a processing pipeline.
//
// Demonstrates composing a Provider with a Pipeline of Processors.
//
// Usage:
//
//	export OPENAI_API_KEY=sk-...
//	go run ./examples/pipeline
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"ryn.dev/ryn"
	"ryn.dev/ryn/provider/openai"
)

func main() {
	ctx := context.Background()

	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY not set")
		os.Exit(1)
	}

	llm := openai.New(key)

	// Generate a streaming response
	stream, err := llm.Generate(ctx, &ryn.Request{
		Model: "gpt-4o",
		Messages: []ryn.Message{
			ryn.Text(ryn.RoleSystem, "You are a concise assistant."),
			ryn.Text(ryn.RoleUser, "List 3 benefits of streaming architectures, one per line."),
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Build a processing pipeline:
	//   1. Filter to text frames only
	//   2. Log each token to stderr
	//   3. Uppercase the output
	pipeline := ryn.Pipe(
		ryn.TextOnly(),

		ryn.Tap(func(f ryn.Frame) {
			fmt.Fprintf(os.Stderr, "[token] %q\n", f.Text)
		}),

		ryn.Map(func(f ryn.Frame) ryn.Frame {
			f.Text = strings.ToUpper(f.Text)
			return f
		}),
	)

	// Run LLM output through the pipeline
	out := pipeline.Run(ctx, stream)

	for out.Next(ctx) {
		fmt.Print(out.Frame().Text)
	}
	fmt.Println()

	if err := out.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "pipeline error: %v\n", err)
		os.Exit(1)
	}
}
