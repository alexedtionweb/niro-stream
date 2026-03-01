// Command tools demonstrates automatic tool-call handling with the tools package.
//
// tools.NewToolLoop manages the request→tool-call→result→request round-trips
// automatically so the caller only iterates one final stream.
//
// Select a provider with the PROVIDER env var (default: openai):
//
//	PROVIDER=openai    OPENAI_API_KEY=sk-...        go run ./tools
//	PROVIDER=anthropic ANTHROPIC_API_KEY=sk-ant-... go run ./tools
//	PROVIDER=gemini    GEMINI_API_KEY=...            go run ./tools
//	PROVIDER=bedrock                                 go run ./tools  # uses ~/.aws
//	PROVIDER=ollama    MODEL=llama3.2                go run ./tools  # local Ollama
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/provider/anthropic"
	"github.com/alexedtionweb/niro-stream/provider/bedrock"
	"github.com/alexedtionweb/niro-stream/provider/compat"
	"github.com/alexedtionweb/niro-stream/provider/google"
	"github.com/alexedtionweb/niro-stream/provider/openai"
	"github.com/alexedtionweb/niro-stream/tools"
	"github.com/aws/aws-sdk-go-v2/config"
)

// weatherArgs is the input schema for the get_weather tool.
type weatherArgs struct {
	City    string `json:"city"`
	Country string `json:"country,omitempty"`
}

func main() {
	ctx := context.Background()

	// Configure slog: text handler writing to stderr.
	// Set LOG_LEVEL=debug to see retry and other library diagnostics.
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	llm := mustProvider(ctx)

	// Define tools using the typed helper — the JSON schema is inferred
	// automatically from the Go struct passed as the third argument.
	weatherDef, err := tools.NewToolDefinitionAny(
		"get_weather",
		"Return the current weather conditions for a city.",
		weatherArgs{},
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			var args weatherArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, err
			}
			return fetchWeather(args), nil
		},
	)
	if err != nil {
		slog.Error("weather tool definition failed", "err", err)
		os.Exit(1)
	}

	timeDef, err := tools.NewToolDefinitionAny(
		"get_current_time",
		"Return the current UTC time.",
		struct{}{},
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			return map[string]string{"utc": time.Now().UTC().Format(time.RFC3339)}, nil
		},
	)
	if err != nil {
		slog.Error("time tool definition failed", "err", err)
		os.Exit(1)
	}

	ts := tools.NewToolset().MustRegister(weatherDef).MustRegister(timeDef)

	fmt.Println("User: What's the weather in Tokyo and Paris right now? Also tell me the current UTC time.")
	fmt.Println()

	// NewToolLoop manages the multi-round tool-call loop automatically.
	// MaxRounds=5 prevents runaway loops if a model keeps calling tools.
	loop := tools.NewToolLoop(ts, 5)
	stream, err := loop.GenerateWithTools(ctx, llm, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("What's the weather in Tokyo and Paris right now? Also tell me the current UTC time.")},

		Tools:   ts.Tools(),
		Options: ryn.Options{MaxTokens: 512},
	})
	if err != nil {
		slog.Error("generate failed", "err", err)
		os.Exit(1)
	}

	for stream.Next(ctx) {
		if f := stream.Frame(); f.Kind == ryn.KindText {
			fmt.Print(f.Text)
		}
	}
	fmt.Println()

	if err := stream.Err(); err != nil {
		slog.Error("stream failed", "err", err)
		os.Exit(1)
	}

	usage := stream.Usage()
	slog.Info("done", "in", usage.InputTokens, "out", usage.OutputTokens, "total", usage.TotalTokens)
}

// fetchWeather simulates a weather API response.
func fetchWeather(args weatherArgs) map[string]any {
	time.Sleep(20 * time.Millisecond) // simulate network latency

	data := map[string]map[string]any{
		"Tokyo":  {"temp_c": 22, "condition": "partly cloudy", "humidity": 65},
		"Paris":  {"temp_c": 15, "condition": "overcast", "humidity": 78},
		"London": {"temp_c": 12, "condition": "rainy", "humidity": 88},
		"Sydney": {"temp_c": 28, "condition": "sunny", "humidity": 52},
	}

	if w, ok := data[args.City]; ok {
		w["city"] = args.City
		return w
	}
	return map[string]any{"city": args.City, "error": "unknown city"}
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
