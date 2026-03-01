// Command multi-provider demonstrates runtime provider selection using the
// registry and multi-tenant routing.
//
// It registers OpenAI, Anthropic, Gemini, and Bedrock under named keys and
// routes requests based on the PROVIDER env var (or falls back to a default).
// This pattern suits applications that let users choose their LLM backend at
// runtime without changing application code.
//
//	OPENAI_API_KEY=sk-... ANTHROPIC_API_KEY=sk-ant-... \
//	  GEMINI_API_KEY=... go run ./multi-provider
//
// Set PROVIDER=openai|anthropic|gemini|bedrock to steer the request, or leave
// it unset to use the configured default.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/provider/anthropic"
	"github.com/alexedtionweb/niro-stream/provider/bedrock"
	"github.com/alexedtionweb/niro-stream/provider/google"
	"github.com/alexedtionweb/niro-stream/provider/openai"
	"github.com/alexedtionweb/niro-stream/registry"
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

	// Build a provider registry.  Only register providers whose credentials
	// are available so the binary works even if some keys are absent.
	reg := registry.New()

	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		reg.Register("openai", openai.New(key,
			openai.WithModel("gpt-4o-mini"),
		))
		slog.Info("provider registered", "name", "openai")
	}

	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		reg.Register("anthropic", anthropic.New(key,
			anthropic.WithModel("claude-3-5-haiku-20241022"),
		))
		slog.Info("provider registered", "name", "anthropic")
	}

	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		p, err := google.New(key, google.WithModel("gemini-2.0-flash"))
		if err != nil {
			slog.Warn("google provider init failed, skipping", "err", err)
		} else {
			reg.Register("gemini", p)
			slog.Info("provider registered", "name", "gemini")
		}
	}

	if awsCfg, err := config.LoadDefaultConfig(ctx); err == nil {
		reg.Register("bedrock", bedrock.New(awsCfg,
			bedrock.WithModel("anthropic.claude-3-5-sonnet-20241022-v2:0"),
		))
		slog.Info("provider registered", "name", "bedrock")
	}

	if reg.Len() == 0 {
		slog.Error("no providers registered", "hint", "set at least one API key env var")
		os.Exit(1)
	}

	fmt.Printf("registered providers: %v\n\n", reg.Names())

	// MultiTenantProvider routes requests to the right backend.
	// Priority: req.Client > PROVIDER env > default > sole registered provider.
	router := registry.NewMultiTenantProvider(reg,
		registry.WithDefaultClient(firstAvailable(reg, "openai", "anthropic", "gemini", "bedrock")),
		registry.WithClientSelector(func(ctx context.Context, req *niro.Request) (string, error) {
			// Allow overriding via env var at runtime.
			if p := os.Getenv("PROVIDER"); p != "" && reg.Has(p) {
				return p, nil
			}
			return "", nil // fall through to req.Client / default
		}),
	)

	// Per-provider request mutators inject auth headers, model IDs, or any
	// provider-specific fields without polluting the shared request object.
	router2 := registry.NewMultiTenantProvider(reg,
		registry.WithDefaultClient(firstAvailable(reg, "openai", "anthropic", "gemini", "bedrock")),
		registry.WithClientMutator("anthropic", func(ctx context.Context, req *niro.Request) error {
			// Example: always use the extended-thinking model for Anthropic.
			if req.Model == "" {
				req.Model = "claude-3-5-sonnet-20241022"
			}
			return nil
		}),
	)
	_ = router2 // shown for illustration; using router below

	prompt := "Name three interesting facts about the Go programming language. Be brief."

	// ── Demo 1: route via env var ──────────────────────────────────────────────
	fmt.Println("--- Request via env PROVIDER ---")
	if err := generate(ctx, router, "", prompt); err != nil {
		slog.Error("generate failed", "err", err)
	}

	// ── Demo 2: explicit per-request client override ──────────────────────────
	for _, provider := range reg.Names() {
		fmt.Printf("\n--- Request routed to: %s ---\n", provider)
		if err := generate(ctx, router, provider, prompt); err != nil {
			slog.Error("generate failed", "provider", provider, "err", err)
		}
	}
}

// generate calls router.Generate, streaming output to stdout.
func generate(ctx context.Context, router niro.Provider, client, prompt string) error {
	stream, err := router.Generate(ctx, &niro.Request{
		Client:   client, // explicit override; empty = use selector / default
		Messages: []niro.Message{niro.UserText(prompt)},
		Options:  niro.Options{MaxTokens: 200, Temperature: niro.Temp(0.5)},
	})
	if err != nil {
		return err
	}

	for stream.Next(ctx) {
		if f := stream.Frame(); f.Kind == niro.KindText {
			fmt.Print(f.Text)
		}
	}
	fmt.Println()

	if err := stream.Err(); err != nil {
		return err
	}

	if resp := stream.Response(); resp != nil {
		slog.Info("response", "model", resp.Model, "finish", resp.FinishReason, "tokens", stream.Usage().TotalTokens)
	}
	return nil
}

// firstAvailable returns the first name in candidates that is registered.
func firstAvailable(reg *registry.Registry, candidates ...string) string {
	for _, c := range candidates {
		if reg.Has(c) {
			return c
		}
	}
	return ""
}
