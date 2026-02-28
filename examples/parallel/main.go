// Command parallel demonstrates Fan, Race, and Sequence orchestration patterns.
//
// Fan runs multiple generations simultaneously and merges their streams.
// Race runs multiple generations simultaneously and keeps the first to finish.
// Sequence chains generations so each step receives the previous step's output.
//
// Select a provider with the PROVIDER env var (default: openai):
//
//	PROVIDER=openai    OPENAI_API_KEY=sk-...        go run ./parallel
//	PROVIDER=anthropic ANTHROPIC_API_KEY=sk-ant-... go run ./parallel
//	PROVIDER=gemini    GEMINI_API_KEY=...            go run ./parallel
//	PROVIDER=bedrock                                 go run ./parallel  # uses ~/.aws
//	PROVIDER=ollama    MODEL=llama3.2                go run ./parallel  # local Ollama
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"ryn.dev/ryn"
	"ryn.dev/ryn/orchestrate"
	"ryn.dev/ryn/provider/anthropic"
	"ryn.dev/ryn/provider/bedrock"
	"ryn.dev/ryn/provider/compat"
	"ryn.dev/ryn/provider/google"
	"ryn.dev/ryn/provider/openai"
)

func main() {
	ctx := context.Background()

	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	llm := mustProvider(ctx)

	fmt.Println("\n=== Fan: parallel generation, merged output ===")
	fanDemo(ctx, llm)

	fmt.Println("\n=== Race: first response wins, others cancelled ===")
	raceDemo(ctx, llm)

	fmt.Println("\n=== Sequence: chained generation (haiku → critique) ===")
	sequenceDemo(ctx, llm)
}

// fanDemo generates two independent responses in parallel and prints them merged.
func fanDemo(ctx context.Context, llm ryn.Provider) {
	gen := func(question string) func(context.Context) (*ryn.Stream, error) {
		return func(ctx context.Context) (*ryn.Stream, error) {
			return llm.Generate(ctx, &ryn.Request{
				SystemPrompt: "Answer in exactly one sentence.",
				Messages:     []ryn.Message{ryn.UserText(question)},
				Options:      ryn.Options{MaxTokens: 80, Temperature: ryn.Temp(0.3)},
			})
		}
	}

	stream := orchestrate.Fan(ctx,
		gen("What is Go?"),
		gen("What is Rust?"),
		gen("What is Zig?"),
	)

	text, err := ryn.CollectText(ctx, stream)
	if err != nil {
		slog.Error("fan failed", "err", err)
		return
	}
	fmt.Println(text)
}

// raceDemo fires three requests at slightly different temperatures and keeps
// the first full response that comes back.
func raceDemo(ctx context.Context, llm ryn.Provider) {
	gen := func(temp float64) func(context.Context) (*ryn.Stream, error) {
		return func(ctx context.Context) (*ryn.Stream, error) {
			return llm.Generate(ctx, &ryn.Request{
				SystemPrompt: "One sentence only.",
				Messages:     []ryn.Message{ryn.UserText("Define concurrency.")},
				Options:      ryn.Options{MaxTokens: 64, Temperature: ryn.Temp(temp)},
			})
		}
	}

	text, usage, err := orchestrate.Race(ctx, gen(0.1), gen(0.5), gen(0.9))
	if err != nil {
		slog.Error("race failed", "err", err)
		return
	}
	fmt.Println(text)
	slog.Info("race winner", "tokens", usage.TotalTokens)
}

// sequenceDemo composes two steps: first write a haiku, then critique it.
// Each step's output becomes the next step's input.
func sequenceDemo(ctx context.Context, llm ryn.Provider) {
	stream, err := orchestrate.Sequence(ctx,
		// Step 1: write a haiku.
		func(ctx context.Context, _ string) (*ryn.Stream, error) {
			return llm.Generate(ctx, &ryn.Request{
				SystemPrompt: "Write exactly one haiku. No extra text.",
				Messages:     []ryn.Message{ryn.UserText("Topic: streaming data")},
				Options:      ryn.Options{MaxTokens: 48},
			})
		},
		// Step 2: critique the haiku produced by step 1.
		func(ctx context.Context, haiku string) (*ryn.Stream, error) {
			return llm.Generate(ctx, &ryn.Request{
				SystemPrompt: "You are a concise poetry critic. One sentence.",
				Messages:     []ryn.Message{ryn.UserText("Critique: " + haiku)},
				Options:      ryn.Options{MaxTokens: 96},
			})
		},
	)
	if err != nil {
		slog.Error("sequence failed", "err", err)
		return
	}

	for stream.Next(ctx) {
		if f := stream.Frame(); f.Kind == ryn.KindText {
			fmt.Print(f.Text)
		}
	}
	fmt.Println()
	if err := stream.Err(); err != nil {
		slog.Error("sequence stream failed", "err", err)
	}
}

// mustProvider returns a Provider for the selected PROVIDER.
func mustProvider(ctx context.Context) ryn.Provider {
	switch strings.ToLower(os.Getenv("PROVIDER")) {
	case "", "openai":
		return openai.New(os.Getenv("OPENAI_API_KEY"))

	case "anthropic":
		return anthropic.New(os.Getenv("ANTHROPIC_API_KEY"))

	case "gemini":
		p, err := google.New(os.Getenv("GEMINI_API_KEY"))
		if err != nil {
			slog.Error("google provider init failed", "err", err)
			os.Exit(1)
		}
		return p

	case "bedrock":
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			slog.Error("aws config load failed", "err", err)
			os.Exit(1)
		}
		model := os.Getenv("MODEL")
		if model == "" {
			model = "anthropic.claude-3-5-sonnet-20241022-v2:0"
		}
		return bedrock.New(cfg, bedrock.WithModel(model))

	case "ollama":
		baseURL := os.Getenv("OLLAMA_BASE_URL")
		if baseURL == "" {
			baseURL = "http://localhost:11434/v1"
		}
		model := os.Getenv("MODEL")
		if model == "" {
			model = "llama3.2"
		}
		return compat.New(baseURL, "", compat.WithModel(model))

	default:
		slog.Error("unknown PROVIDER", "provider", os.Getenv("PROVIDER"),
			"valid", "openai|anthropic|gemini|bedrock|ollama")
		os.Exit(1)
		panic("unreachable")
	}
}
