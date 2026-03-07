// Command bedrock demonstrates streaming generation and tool calling with
// Amazon Bedrock using the AWS SDK v2 default credential chain.
//
// Credentials are resolved automatically from the environment:
//
//   - ~/.aws/credentials or ~/.aws/config
//
//   - AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN env vars
//
//   - IAM instance / task role
//
//     AWS_REGION=us-east-1 go run ./bedrock
//
// Override the model:
//
//	MODEL=amazon.nova-pro-v1:0 AWS_REGION=us-east-1 go run ./bedrock
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/provider/bedrock"
	"github.com/alexedtionweb/niro-stream/tools"
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

	// Load AWS credentials from the default provider chain.
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		slog.Error("aws config load failed", "err", err)
		os.Exit(1)
	}

	model := os.Getenv("MODEL")
	if model == "" {
		model = "anthropic.claude-3-5-sonnet-20241022-v2:0"
	}

	llm := bedrock.New(awsCfg, bedrock.WithModel(model))

	fmt.Printf("Provider : Amazon Bedrock\nModel    : %s\n\n", model)

	// ── Demo 1: basic streaming chat ──────────────────────────────────────────
	fmt.Println("=== Basic streaming chat ===")
	basicChat(ctx, llm)

	// ── Demo 2: multi-turn conversation ───────────────────────────────────────
	fmt.Println("\n=== Multi-turn conversation ===")
	multiTurn(ctx, llm)

	// ── Demo 3: tool calling ──────────────────────────────────────────────────
	fmt.Println("\n=== Tool calling ===")
	toolCalling(ctx, llm)
}

func basicChat(ctx context.Context, llm niro.Provider) {
	stream, err := llm.Generate(ctx, &niro.Request{
		SystemPrompt: "You are a helpful assistant. Keep answers short.",
		Messages: []niro.Message{
			niro.UserText("What makes Amazon Bedrock different from calling model APIs directly?"),
		},
		Options: niro.Options{MaxTokens: 256, Temperature: niro.Temp(0.5)},
	})
	if err != nil {
		slog.Error("generate failed", "err", err)
		return
	}

	for stream.Next(ctx) {
		if f := stream.Frame(); f.Kind == niro.KindText {
			fmt.Print(f.Text)
		}
	}
	fmt.Println()
	if err := stream.Err(); err != nil {
		slog.Error("stream failed", "err", err)
	}
	printUsage(stream)
}

func multiTurn(ctx context.Context, llm niro.Provider) {
	conversation := []niro.Message{
		niro.UserText("I'm learning about cloud architectures. Start by explaining serverless in one sentence."),
	}

	nextQuestions := []string{
		"Now explain containers in one sentence.",
		"How do the two compare for latency-sensitive workloads? One sentence.",
	}

	for i := 0; i <= len(nextQuestions); i++ {
		stream, err := llm.Generate(ctx, &niro.Request{
			SystemPrompt: "You are a cloud architecture expert. Be concise.",
			Messages:     conversation,
			Options:      niro.Options{MaxTokens: 128},
		})
		if err != nil {
			slog.Error("turn failed", "turn", i, "err", err)
			return
		}

		var reply strings.Builder
		for stream.Next(ctx) {
			if f := stream.Frame(); f.Kind == niro.KindText {
				fmt.Print(f.Text)
				reply.WriteString(f.Text)
			}
		}
		fmt.Println()
		if err := stream.Err(); err != nil {
			slog.Error("stream failed", "turn", i, "err", err)
			return
		}

		conversation = append(conversation, niro.AssistantText(reply.String()))
		if i < len(nextQuestions) {
			conversation = append(conversation, niro.UserText(nextQuestions[i]))
		}
	}
}

func toolCalling(ctx context.Context, llm niro.Provider) {
	type stockArgs struct {
		Symbol string `json:"symbol"`
	}

	stockDef, err := tools.NewToolDefinitionAny(
		"get_stock_price",
		"Return the latest stock price for a ticker symbol.",
		stockArgs{},
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			var args stockArgs
			if err := niro.JSONUnmarshal(raw, &args); err != nil {
				return nil, err
			}
			prices := map[string]float64{
				"AMZN": 185.42, "GOOG": 172.18, "MSFT": 415.66, "NVDA": 875.39,
			}
			if p, ok := prices[args.Symbol]; ok {
				return map[string]any{"symbol": args.Symbol, "price": p, "currency": "USD"}, nil
			}
			return map[string]any{"symbol": args.Symbol, "error": "unknown ticker"}, nil
		},
	)
	if err != nil {
		slog.Error("tool definition failed", "err", err)
		return
	}

	ts := tools.NewToolset().MustRegister(stockDef)

	loop := tools.NewToolLoop(ts, 5)
	stream, err := loop.GenerateWithTools(ctx, llm, &niro.Request{
		Messages: []niro.Message{
			niro.UserText("What are the current stock prices for AMZN and NVDA?"),
		},
		Tools:   ts.Tools(),
		Options: niro.Options{MaxTokens: 256},
	})
	if err != nil {
		slog.Error("generate failed", "err", err)
		return
	}

	for stream.Next(ctx) {
		if f := stream.Frame(); f.Kind == niro.KindText {
			fmt.Print(f.Text)
		}
	}
	fmt.Println()
	if err := stream.Err(); err != nil {
		slog.Error("stream failed", "err", err)
	}
	printUsage(stream)
}

func printUsage(s *niro.Stream) {
	u := s.Usage()
	if u.TotalTokens > 0 {
		slog.Info("usage", "in", u.InputTokens, "out", u.OutputTokens, "total", u.TotalTokens)
	}
}
