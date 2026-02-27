// Command chat demonstrates basic streaming chat with multiple providers.
//
// Usage:
//
//	OPENAI_API_KEY=sk-... go run ./examples/chat
//	ANTHROPIC_API_KEY=sk-... PROVIDER=anthropic go run ./examples/chat
//	PROVIDER=compat COMPAT_BASE_URL=http://localhost:11434/v1 go run ./examples/chat
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"ryn.dev/ryn"
	"ryn.dev/ryn/provider/anthropic"
	"ryn.dev/ryn/provider/compat"
	"ryn.dev/ryn/provider/openai"
)

func main() {
	ctx := context.Background()

	llm := selectProvider()

	stream, err := llm.Generate(ctx, &ryn.Request{
		SystemPrompt: "You are a helpful assistant. Be concise.",
		Messages:     []ryn.Message{ryn.UserText("Explain Go channels in 3 sentences.")},
		Options: ryn.Options{
			MaxTokens:   256,
			Temperature: ryn.Temp(0.7),
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate: %v\n", err)
		os.Exit(1)
	}

	// Stream tokens to stdout
	for stream.Next(ctx) {
		f := stream.Frame()
		if f.Kind == ryn.KindText {
			fmt.Print(f.Text)
		}
	}
	fmt.Println()

	if err := stream.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "\nstream error: %v\n", err)
		os.Exit(1)
	}

	// Print usage
	usage := stream.Usage()
	fmt.Fprintf(os.Stderr, "\n--- Usage: in=%d out=%d total=%d\n",
		usage.InputTokens, usage.OutputTokens, usage.TotalTokens)

	if resp := stream.Response(); resp != nil {
		fmt.Fprintf(os.Stderr, "--- Model: %s  Finish: %s\n", resp.Model, resp.FinishReason)
	}
}

func selectProvider() ryn.Provider {
	provider := strings.ToLower(os.Getenv("PROVIDER"))
	switch provider {
	case "anthropic":
		return anthropic.New(os.Getenv("ANTHROPIC_API_KEY"))
	case "compat":
		url := os.Getenv("COMPAT_BASE_URL")
		if url == "" {
			url = "http://localhost:11434/v1"
		}
		return compat.New(url, os.Getenv("COMPAT_API_KEY"))
	default:
		return openai.New(os.Getenv("OPENAI_API_KEY"))
	}
}
