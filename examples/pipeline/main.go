// Command pipeline demonstrates frame processing pipelines and observability hooks.
//
// The pipeline runs three stages:
//  1. TextOnly  — drops non-text frames
//  2. Map       — transforms each text token
//  3. Accumulate — collapses the token stream into one frame
//
// Hooks log timing and token usage to stderr.
//
// Select a provider with the PROVIDER env var (default: openai):
//
//	PROVIDER=openai    OPENAI_API_KEY=sk-...        go run ./pipeline
//	PROVIDER=anthropic ANTHROPIC_API_KEY=sk-ant-... go run ./pipeline
//	PROVIDER=gemini    GEMINI_API_KEY=...            go run ./pipeline
//	PROVIDER=bedrock                                 go run ./pipeline  # uses ~/.aws
//	PROVIDER=ollama    MODEL=llama3.2                go run ./pipeline  # local Ollama
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/hook"
	"github.com/alexedtionweb/niro-stream/pipe"
	"github.com/alexedtionweb/niro-stream/provider/anthropic"
	"github.com/alexedtionweb/niro-stream/provider/bedrock"
	"github.com/alexedtionweb/niro-stream/provider/compat"
	"github.com/alexedtionweb/niro-stream/provider/google"
	"github.com/alexedtionweb/niro-stream/provider/openai"
	"github.com/alexedtionweb/niro-stream/runtime"
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

	llm := mustProvider(ctx)

	// Stage 1: only pass text frames through.
	// Stage 2: upper-case each token so the effect is visible.
	// Stage 3: collapse the stream into a single aggregated frame.
	pipeline := pipe.New(
		pipe.TextOnly(),
		pipe.Map(func(f ryn.Frame) ryn.Frame {
			f.Text = strings.ToUpper(f.Text)
			return f
		}),
		pipe.Accumulate(),
	).WithBuffer(32)

	// Hook logs start/end/error events via slog.
	h := &logHook{}

	rt := runtime.New(llm).
		WithPipeline(pipeline).
		WithHook(h)

	stream, err := rt.Generate(ctx, &ryn.Request{
		SystemPrompt: "You are a helpful assistant.",
		Messages:     []ryn.Message{ryn.UserText("List three benefits of Go in one sentence each.")},
		Options:      ryn.Options{MaxTokens: 128},
	})
	if err != nil {
		slog.Error("generate failed", "err", err)
		os.Exit(1)
	}

	for stream.Next(ctx) {
		fmt.Print(stream.Frame().Text)
	}
	fmt.Println()

	if err := stream.Err(); err != nil {
		slog.Error("stream failed", "err", err)
		os.Exit(1)
	}
}

// logHook emits structured log lines via slog for each lifecycle event.
type logHook struct{ hook.NoOpHook }

func (h *logHook) OnGenerateStart(ctx context.Context, info hook.GenerateStartInfo) context.Context {
	slog.InfoContext(ctx, "generate start",
		"model", info.Model,
		"messages", info.Messages,
		"tools", info.Tools)
	return ctx
}

func (h *logHook) OnGenerateEnd(ctx context.Context, info hook.GenerateEndInfo) {
	slog.InfoContext(ctx, "generate end",
		"model", info.Model,
		"dur", info.Duration.Round(time.Millisecond),
		"in", info.Usage.InputTokens,
		"out", info.Usage.OutputTokens,
		"finish", info.FinishReason)
}

func (h *logHook) OnError(ctx context.Context, err error) {
	slog.ErrorContext(ctx, "generate error", "err", err)
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
