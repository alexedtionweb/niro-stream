// Streaming chat — minimal Ryn example.
//
// Usage:
//
//	export OPENAI_API_KEY=sk-...
//	go run ./examples/chat
package main

import (
	"context"
	"fmt"
	"os"

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

	stream, err := llm.Generate(ctx, &ryn.Request{
		Model: "gpt-4o",
		Messages: []ryn.Message{
			ryn.Text(ryn.RoleSystem, "You are a concise assistant. Respond in 1-2 sentences."),
			ryn.Text(ryn.RoleUser, "What makes streaming architectures different from request-response?"),
		},
		Options: ryn.Options{
			Temperature: ryn.Temp(0.7),
			MaxTokens:   256,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	for stream.Next(ctx) {
		f := stream.Frame()
		if f.Kind == ryn.KindText {
			fmt.Print(f.Text)
		}
	}
	fmt.Println()

	if err := stream.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "stream error: %v\n", err)
		os.Exit(1)
	}
}
