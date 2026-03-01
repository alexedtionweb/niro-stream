// Command gemini demonstrates streaming generation, structured JSON output,
// and multi-turn conversations.
//
// Select a provider with the PROVIDER env var (default: openai):
//
//	PROVIDER=openai    OPENAI_API_KEY=sk-...        go run ./gemini
//	PROVIDER=anthropic ANTHROPIC_API_KEY=sk-ant-... go run ./gemini
//	PROVIDER=gemini    GEMINI_API_KEY=...            go run ./gemini
//	PROVIDER=bedrock                                 go run ./gemini  # uses ~/.aws
//	PROVIDER=ollama    MODEL=llama3.2                go run ./gemini  # local Ollama
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/provider/anthropic"
	"github.com/alexedtionweb/niro-stream/provider/bedrock"
	"github.com/alexedtionweb/niro-stream/provider/compat"
	"github.com/alexedtionweb/niro-stream/provider/google"
	"github.com/alexedtionweb/niro-stream/provider/openai"
	"github.com/alexedtionweb/niro-stream/structured"
	"github.com/aws/aws-sdk-go-v2/config"
)

func main() {
	ctx := context.Background()

	// Configure slog: text handler writing to stderr.
	// Set LOG_LEVEL=debug to see retry and other library diagnostics.
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	p := mustProvider(ctx)

	// ── Demo 1: basic streaming ────────────────────────────────────────────────
	fmt.Println("=== Basic streaming ===")
	basicStreaming(ctx, p)

	// ── Demo 2: structured JSON output ────────────────────────────────────────
	fmt.Println("\n=== Structured JSON output ===")
	structuredOutput(ctx, p)

	// ── Demo 3: multi-turn conversation ───────────────────────────────────────
	fmt.Println("\n=== Multi-turn conversation ===")
	multiTurn(ctx, p)
}

func basicStreaming(ctx context.Context, p ryn.Provider) {
	stream, err := p.Generate(ctx, &ryn.Request{
		SystemPrompt: "You are a helpful assistant. Be concise.",
		Messages: []ryn.Message{
			ryn.UserText("Explain how large language models generate text, in two sentences."),
		},
		Options: ryn.Options{MaxTokens: 200, Temperature: ryn.Temp(0.6)},
	})
	if err != nil {
		slog.Error("generate failed", "err", err)
		return
	}

	for stream.Next(ctx) {
		if f := stream.Frame(); f.Kind == ryn.KindText {
			fmt.Print(f.Text)
		}
	}
	fmt.Println()
	if err := stream.Err(); err != nil {
		slog.Error("stream failed", "err", err)
	}
	printUsage(stream)
}

// Recipe is the schema for structured JSON output.
type Recipe struct {
	Name        string   `json:"name"`
	Ingredients []string `json:"ingredients"`
	Steps       []string `json:"steps"`
	PrepMinutes int      `json:"prep_minutes"`
}

// recipeSchema is the JSON Schema for Recipe.
var recipeSchema = json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"ingredients":{"type":"array","items":{"type":"string"}},"steps":{"type":"array","items":{"type":"string"}},"prep_minutes":{"type":"integer"}},"required":["name","ingredients","steps","prep_minutes"]}`)

func structuredOutput(ctx context.Context, p ryn.Provider) {
	req := structured.WithSchema(&ryn.Request{
		Messages: []ryn.Message{ryn.UserText("Give me a simple pasta recipe.")},
		Options:  ryn.Options{MaxTokens: 512},
	}, recipeSchema)

	recipe, _, _, err := structured.GenerateStructured[Recipe](ctx, p, req, recipeSchema)
	if err != nil {
		slog.Error("structured generation failed", "err", err)
		return
	}

	b, _ := json.MarshalIndent(recipe, "", "  ")
	fmt.Println(string(b))
}

func multiTurn(ctx context.Context, p ryn.Provider) {
	conversation := []ryn.Message{
		ryn.UserText("What is the capital of France?"),
	}

	followUps := []string{
		"What is its most famous landmark?",
		"How many tourists visit it each year? One sentence.",
	}

	for i := 0; i <= len(followUps); i++ {
		stream, err := p.Generate(ctx, &ryn.Request{
			SystemPrompt: "You are a knowledgeable travel guide. Keep answers to one or two sentences.",
			Messages:     conversation,
			Options:      ryn.Options{MaxTokens: 128},
		})
		if err != nil {
			slog.Error("turn failed", "turn", i, "err", err)
			return
		}

		var reply strings.Builder
		for stream.Next(ctx) {
			if f := stream.Frame(); f.Kind == ryn.KindText {
				fmt.Print(f.Text)
				reply.WriteString(f.Text)
			}
		}
		fmt.Println()

		conversation = append(conversation, ryn.AssistantText(reply.String()))
		if i < len(followUps) {
			conversation = append(conversation, ryn.UserText(followUps[i]))
		}
	}
}

func printUsage(s *ryn.Stream) {
	u := s.Usage()
	if u.TotalTokens > 0 {
		slog.Info("usage", "in", u.InputTokens, "out", u.OutputTokens, "total", u.TotalTokens)
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
