// Command parallel demonstrates Fan, Race, and Sequence orchestration.
//
// Usage:
//
//	OPENAI_API_KEY=sk-... go run ./examples/parallel
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

	llm := openai.New(os.Getenv("OPENAI_API_KEY"))

	fmt.Println("=== Fan: Parallel generation, merged stream ===")
	fanDemo(ctx, llm)

	fmt.Println("\n=== Race: First response wins ===")
	raceDemo(ctx, llm)

	fmt.Println("\n=== Sequence: Chained generation ===")
	sequenceDemo(ctx, llm)
}

func fanDemo(ctx context.Context, llm ryn.Provider) {
	// Generate two responses in parallel and merge them
	stream := ryn.Fan(ctx,
		func(ctx context.Context) (*ryn.Stream, error) {
			return llm.Generate(ctx, &ryn.Request{
				SystemPrompt: "Respond in exactly one sentence.",
				Messages:     []ryn.Message{ryn.UserText("What is Go?")},
				Options:      ryn.Options{MaxTokens: 64, Temperature: ryn.Temp(0.3)},
			})
		},
		func(ctx context.Context) (*ryn.Stream, error) {
			return llm.Generate(ctx, &ryn.Request{
				SystemPrompt: "Respond in exactly one sentence.",
				Messages:     []ryn.Message{ryn.UserText("What is Rust?")},
				Options:      ryn.Options{MaxTokens: 64, Temperature: ryn.Temp(0.3)},
			})
		},
	)

	text, err := ryn.CollectText(ctx, stream)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fan: %v\n", err)
		return
	}
	fmt.Println(text)
}

func raceDemo(ctx context.Context, llm ryn.Provider) {
	gen := func(temp float64) func(context.Context) (*ryn.Stream, error) {
		return func(ctx context.Context) (*ryn.Stream, error) {
			return llm.Generate(ctx, &ryn.Request{
				SystemPrompt: "One sentence only.",
				Messages:     []ryn.Message{ryn.UserText("What is concurrency?")},
				Options:      ryn.Options{MaxTokens: 64, Temperature: ryn.Temp(temp)},
			})
		}
	}

	text, usage, err := ryn.Race(ctx, gen(0.2), gen(0.8), gen(1.0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "race: %v\n", err)
		return
	}
	fmt.Printf("Winner: %s\n", text)
	fmt.Fprintf(os.Stderr, "--- Usage: in=%d out=%d\n", usage.InputTokens, usage.OutputTokens)
}

func sequenceDemo(ctx context.Context, llm ryn.Provider) {
	stream, err := ryn.Sequence(ctx,
		// Step 1: generate a haiku
		func(ctx context.Context, _ string) (*ryn.Stream, error) {
			return llm.Generate(ctx, &ryn.Request{
				SystemPrompt: "Write exactly one haiku.",
				Messages:     []ryn.Message{ryn.UserText("Topic: streaming data")},
				Options:      ryn.Options{MaxTokens: 64},
			})
		},
		// Step 2: critique the haiku
		func(ctx context.Context, haiku string) (*ryn.Stream, error) {
			return llm.Generate(ctx, &ryn.Request{
				SystemPrompt: "You are a poetry critic. Give a one-line critique.",
				Messages:     []ryn.Message{ryn.UserText("Critique this haiku:\n" + haiku)},
				Options:      ryn.Options{MaxTokens: 128},
			})
		},
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sequence: %v\n", err)
		return
	}

	// Stream the final step
	for stream.Next(ctx) {
		if stream.Frame().Kind == ryn.KindText {
			fmt.Print(stream.Frame().Text)
		}
	}
	fmt.Println()
}
