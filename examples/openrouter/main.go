// Command openrouter demonstrates chat, image, video, and decisions via
// github.com/alexedtionweb/niro-stream/provider/openrouter.
//
//	OPENROUTER_API_KEY=sk-or-... go run ./openrouter chat
//	OPENROUTER_API_KEY=sk-or-... go run ./openrouter image -prompt "A serene mountain landscape"
//	OPENROUTER_API_KEY=sk-or-... go run ./openrouter image -out ./out.webp
//	OPENROUTER_API_KEY=sk-or-... go run ./openrouter video -prompt "A cat walking through neon rain"
//	OPENROUTER_API_KEY=sk-or-... go run ./openrouter decide
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/provider/openrouter"
)

func main() {
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cmd := "chat"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "chat":
		runChat(args)
	case "image":
		runImage(args)
	case "video":
		runVideo(args)
	case "decide":
		runDecide(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "OpenRouter + niro-stream example")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  go run ./openrouter chat   [-model …] [-prompt …]")
	fmt.Fprintln(os.Stderr, "  go run ./openrouter image  [-model …] [-prompt …] [-out PATH]")
	fmt.Fprintln(os.Stderr, "  go run ./openrouter video  [-model …] [-prompt …] [-out PATH]")
	fmt.Fprintln(os.Stderr, "  go run ./openrouter decide [-model …] [-state …]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Env: OPENROUTER_API_KEY (required)")
}

func mustClient() *openrouter.Provider {
	apiKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: OPENROUTER_API_KEY is required")
		os.Exit(1)
	}
	return openrouter.New(apiKey,
		openrouter.WithHTTPReferer("https://github.com/alexedtionweb/niro-stream"),
		openrouter.WithXTitle("niro-stream example"),
	)
}

func runChat(args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	model := fs.String("model", envOr("MODEL", "openai/gpt-4o-mini"), "chat model slug")
	prompt := fs.String("prompt", envOr("PROMPT", "Explain what OpenRouter is in two sentences."), "user prompt")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")
	_ = fs.Parse(args)

	llm := mustClient()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	fmt.Fprintf(os.Stderr, "model=%s\n── reply ──\n", *model)
	stream, err := llm.Generate(ctx, &niro.Request{
		Model:        *model,
		SystemPrompt: "You are a helpful assistant. Be concise.",
		Messages:     []niro.Message{niro.UserText(*prompt)},
		Options:      niro.Options{MaxTokens: 300, Temperature: niro.Temp(0.7)},
	})
	if err != nil {
		fatal(err)
	}
	for stream.Next(ctx) {
		if f := stream.Frame(); f.Kind == niro.KindText {
			fmt.Print(f.Text)
		}
	}
	fmt.Println()
	if err := stream.Err(); err != nil {
		fatal(err)
	}
}

func runImage(args []string) {
	fs := flag.NewFlagSet("image", flag.ExitOnError)
	model := fs.String("model", envOr("MODEL", "recraft/recraft-v4.1-flash-v2:free"), "image model slug")
	prompt := fs.String("prompt", envOr("PROMPT", "A serene mountain landscape at sunset with dramatic clouds"), "image prompt")
	outPath := fs.String("out", "openrouter-image.bin", "output file path")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")
	_ = fs.Parse(args)

	llm := mustClient()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	fmt.Fprintf(os.Stderr, "model=%s\n── image ──\n", *model)
	stream, err := llm.GenerateImage(ctx, &niro.ImageRequest{
		Model:  *model,
		Prompt: *prompt,
	})
	if err != nil {
		fatal(err)
	}

	var saved string
	n := 0
	for stream.Next(ctx) {
		f := stream.Frame()
		switch f.Kind {
		case niro.KindImage:
			path := *outPath
			if n > 0 {
				path = numberedPath(*outPath, n)
			}
			if err := os.WriteFile(path, f.Data, 0o644); err != nil {
				fatal(err)
			}
			fmt.Fprintf(os.Stderr, "wrote %s (%s, %d bytes)\n", path, f.Mime, len(f.Data))
			saved = path
			n++
		case niro.KindUsage:
			if f.Usage != nil && f.Usage.Detail[niro.UsageCostMicros] > 0 {
				fmt.Fprintf(os.Stderr, "cost≈$%.6f\n", float64(f.Usage.Detail[niro.UsageCostMicros])/1e6)
			}
		}
	}
	if err := stream.Err(); err != nil {
		fatal(err)
	}
	if saved == "" {
		fatal(fmt.Errorf("no image frames received"))
	}
}

func runVideo(args []string) {
	fs := flag.NewFlagSet("video", flag.ExitOnError)
	model := fs.String("model", envOr("MODEL", "google/veo-3"), "video model slug")
	prompt := fs.String("prompt", envOr("PROMPT", "A cat walking through neon rain"), "video prompt")
	outPath := fs.String("out", "openrouter-video.mp4", "output file path")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall timeout")
	_ = fs.Parse(args)

	llm := mustClient()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	fmt.Fprintf(os.Stderr, "model=%s\n── video ──\n", *model)
	stream, err := llm.GenerateVideo(ctx, &niro.VideoRequest{
		Model:  *model,
		Prompt: *prompt,
	})
	if err != nil {
		fatal(err)
	}

	var saved string
	n := 0
	for stream.Next(ctx) {
		f := stream.Frame()
		switch f.Kind {
		case niro.KindCustom:
			if f.Custom != nil && f.Custom.Type == niro.CustomVideoStatus {
				if st, ok := f.Custom.Data.(niro.VideoStatus); ok {
					fmt.Fprintf(os.Stderr, "status=%s job=%s\n", st.Status, st.JobID)
				}
			}
		case niro.KindVideo:
			path := *outPath
			if n > 0 {
				path = numberedPath(*outPath, n)
			}
			if err := os.WriteFile(path, f.Data, 0o644); err != nil {
				fatal(err)
			}
			fmt.Fprintf(os.Stderr, "wrote %s (%s, %d bytes)\n", path, f.Mime, len(f.Data))
			saved = path
			n++
		}
	}
	if err := stream.Err(); err != nil {
		fatal(err)
	}
	if saved == "" {
		fatal(fmt.Errorf("no video frames received"))
	}
}

func runDecide(args []string) {
	fs := flag.NewFlagSet("decide", flag.ExitOnError)
	model := fs.String("model", envOr("MODEL", "typesafe/jev-1.13"), "decisions model slug")
	state := fs.String("state", envOr("STATE", "My checkout page shows a blank screen after I click Pay. I have tried two browsers."), "content to evaluate")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")
	_ = fs.Parse(args)

	llm := mustClient()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	fmt.Fprintf(os.Stderr, "model=%s\n── decide ──\n", *model)
	res, err := llm.Decide(ctx, &niro.DecisionRequest{
		Model: *model,
		StateData: map[string]any{
			"customer_tier": "enterprise",
			"ticket":        *state,
		},
		Questions: map[string]niro.DecisionQuestion{
			"is_bug": niro.Noul(
				"Is the customer reporting a software defect?",
				"The customer describes broken or unexpected product behavior.",
				"The customer is asking a question or requesting a feature.",
			),
			"team": niro.Choice("Which team should own this ticket?", map[string]string{
				"account":  "Login, permissions, or profile issues.",
				"frontend": "Rendering, layout, or browser compatibility issues.",
				"payments": "Checkout, billing, or payment processing issues.",
			}),
			"urgency": niro.Score(
				"How urgent is this ticket?",
				"Can wait for the next release",
				"Should be fixed this week",
				"Blocking revenue right now",
			),
		},
	})
	if err != nil {
		fatal(err)
	}

	for id, ans := range res.Answers {
		switch ans.Kind {
		case niro.DecisionNoul:
			fmt.Printf("%s: noul=%.3f true=%v\n", id, ans.Noul, ans.True())
		case niro.DecisionChoice:
			fmt.Printf("%s: choice=%s\n", id, ans.Choice)
		case niro.DecisionScore:
			fmt.Printf("%s: score=%.3f\n", id, ans.Score)
		case niro.DecisionRefusal:
			fmt.Printf("%s: refused\n", id)
		default:
			fmt.Printf("%s: kind=%s\n", id, ans.Kind)
		}
	}
	if res.Usage.Detail[niro.UsageCostMicros] > 0 {
		fmt.Fprintf(os.Stderr, "cost≈$%.6f\n", float64(res.Usage.Detail[niro.UsageCostMicros])/1e6)
	}
}

func numberedPath(path string, n int) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	return fmt.Sprintf("%s-%d%s", base, n, ext)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func fatal(err error) {
	slog.Error("failed", "err", err)
	os.Exit(1)
}
