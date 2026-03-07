package main

import (
	"bufio"
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

// ─────────────────────────────────────────────────────────────
// Tool argument structs (still used internally)
// ─────────────────────────────────────────────────────────────

type deleteArgs struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

type readArgs struct {
	Path string `json:"path"`
}

// ─────────────────────────────────────────────────────────────
// HITL Terminal Approver
// ─────────────────────────────────────────────────────────────

type TerminalApprover struct {
	reader *bufio.Reader
}

func NewTerminalApprover() *TerminalApprover {
	return &TerminalApprover{reader: bufio.NewReader(os.Stdin)}
}

func (ta *TerminalApprover) Approve(ctx context.Context, call niro.ToolCall) (tools.ToolApproval, error) {
	var pretty strings.Builder
	if len(call.Args) > 0 {
		var v any
		if err := json.Unmarshal(call.Args, &v); err == nil {
			b, _ := json.MarshalIndent(v, "    ", "  ")
			pretty.WriteString(string(b))
		} else {
			pretty.WriteString(string(call.Args))
		}
	}

	fmt.Printf("\n┌─ HITL APPROVAL REQUIRED ─────────────────────────────\n")
	fmt.Printf("│  Tool   : %s\n", call.Name)
	fmt.Printf("│  Call ID: %s\n", call.ID)
	if pretty.Len() > 0 {
		fmt.Printf("│  Args   :\n    %s\n", pretty.String())
	}
	fmt.Printf("└───────────────────────────────────────────────────────\n")
	fmt.Printf("  Approve? [y/N] ")

	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := ta.reader.ReadString('\n')
		ch <- result{strings.TrimSpace(strings.ToLower(line)), err}
	}()

	select {
	case <-ctx.Done():
		return tools.ToolApproval{}, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return tools.ToolApproval{}, r.err
		}
		if r.line == "y" || r.line == "yes" {
			fmt.Println("  ✓ Approved")
			return tools.ToolApproval{Approved: true}, nil
		}
		fmt.Println("  ✗ Denied")
		return tools.ToolApproval{
			Approved: false,
			Reason:   "the operator denied this action at the terminal",
		}, nil
	}
}

// ─────────────────────────────────────────────────────────────
// Tool handlers
// ─────────────────────────────────────────────────────────────

func handleDelete(_ context.Context, raw json.RawMessage) (any, error) {
	var args deleteArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	time.Sleep(30 * time.Millisecond)

	return map[string]any{
		"deleted":   args.Path,
		"recursive": args.Recursive,
		"bytes":     4096,
	}, nil
}

func handleRead(_ context.Context, raw json.RawMessage) (any, error) {
	var args readArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	time.Sleep(10 * time.Millisecond)

	return map[string]any{
		"path":    args.Path,
		"content": "# Config\nversion: 2\nlog_level: info\n",
		"size":    38,
	}, nil
}

// ─────────────────────────────────────────────────────────────
// MAIN
// ─────────────────────────────────────────────────────────────

func main() {
	ctx := context.Background()

	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	llm := mustProvider(ctx)

	ts := tools.NewToolset().WithApprover(NewTerminalApprover())

	// ─────────────────────────────────────────────
	// FIX: Use explicit JSON Schema (Gemini-safe)
	// ─────────────────────────────────────────────

	deleteDef, err := tools.NewToolDefinitionAny(
		"delete_file",
		"Permanently delete a file or directory from the file system.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": { "type": "string", "description": "File path to delete" },
				"recursive": { "type": "boolean", "description": "Delete directories recursively" }
			},
			"required": ["path"]
		}`),
		handleDelete,
	)
	if err != nil {
		slog.Error("define delete tool", "err", err)
		os.Exit(1)
	}

	readDef, err := tools.NewToolDefinitionAny(
		"read_file",
		"Read the contents of a file from the file system.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": { "type": "string", "description": "File path to read" }
			},
			"required": ["path"]
		}`),
		handleRead,
	)
	if err != nil {
		slog.Error("define read tool", "err", err)
		os.Exit(1)
	}

	ts.MustRegister(deleteDef).MustRegister(readDef)

	userMsg := "Please read /etc/app/config.yaml and then delete /var/log/app/error.log."

	fmt.Printf("User: %s\n\n", userMsg)
	fmt.Println("(The terminal will prompt for approval before each tool call.)\n")

	loop := tools.NewToolLoopWithOptions(ts, tools.ToolStreamOptions{
		MaxRounds:       6,
		Parallel:        false,
		EmitToolResults: false,
		ToolTimeout:     30 * time.Second,
	})

	stream, err := loop.GenerateWithTools(ctx, llm, &niro.Request{
		Messages: []niro.Message{niro.UserText(userMsg)},
		Tools:    ts.Tools(),
		Options:  niro.Options{MaxTokens: 512},
	})
	if err != nil {
		slog.Error("generate failed", "err", err)
		os.Exit(1)
	}

	fmt.Print("\nAssistant: ")
	for stream.Next(ctx) {
		if f := stream.Frame(); f.Kind == niro.KindText {
			fmt.Print(f.Text)
		}
	}
	fmt.Println()

	if err := stream.Err(); err != nil {
		slog.Error("stream failed", "err", err)
		os.Exit(1)
	}

	u := stream.Usage()
	slog.Info("done", "total_tokens", u.TotalTokens)
}

// ─────────────────────────────────────────────────────────────
// Provider factory (unchanged)
// ─────────────────────────────────────────────────────────────

func mustProvider(ctx context.Context) niro.Provider {
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
		slog.Error("unknown PROVIDER", "provider", os.Getenv("PROVIDER"))
		os.Exit(1)
		return nil
	}
}
