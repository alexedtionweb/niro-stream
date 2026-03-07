// Command dsl is an interactive chat using the agent DSL plugin.
//
// It loads the agent definition and workflow from JSON files (agents.json and
// workflow.json by default). The agent has a real HTTP tool (ViaCEP) for
// Brazilian address lookup; you can ask anything and use tools when relevant.
//
// Files (override with AGENTS_FILE and WORKFLOW_FILE):
//
//	agents.json   — tools (e.g. type "http" lookup_cep) and agents. Tool refs
//	                may use "when" and "unless" (expr expressions over RunContext:
//	                session, event, history, etc.) to include tools conditionally.
//	workflow.json — workflow definitions (e.g. chat = sequence [assistant])
//
// Default file names are looked up in the current directory, then in
// examples/dsl/ (so "go run ./examples/dsl" from the repo root works).
//
//	PROVIDER=openai OPENAI_API_KEY=sk-... go run ./examples/dsl
//	cd examples/dsl && PROVIDER=ollama MODEL=llama3.2 go run .
//
// Commands: /help, /clear, /history, /usage, /quit
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/output"
	"github.com/alexedtionweb/niro-stream/plugin/dsl"
	"github.com/alexedtionweb/niro-stream/provider/bedrock"
	"github.com/alexedtionweb/niro-stream/provider/compat"
	"github.com/alexedtionweb/niro-stream/provider/google"
	"github.com/aws/aws-sdk-go-v2/config"
)

const (
	colReset  = "\033[0m"
	colBold   = "\033[1m"
	colDim    = "\033[2m"
	colCyan   = "\033[36m"
	colGreen  = "\033[32m"
	colYellow = "\033[33m"
	colRed    = "\033[31m"
)

func col(c, s string) string {
	if noColour {
		return s
	}
	return c + s + colReset
}

var noColour = os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb"

// stepLog logs one pipeline step. Message is the step name; attrs are key=value. All steps use Info so the flow is visible by default.
// Steps: startup, turn_start, agent_start, tool_call, tool_result, stream_end, agent_end, handoff_start, handoff_end, turn_end, error.
func stepLog(step string, attrs ...any) { slog.Info(step, attrs...) }

// ── Config & paths ────────────────────────────────────────────────────────────

// resolveConfigPath returns a path to name, trying the current directory first,
// then examples/dsl/ so "go run ./examples/dsl" from repo root works.
func resolveConfigPath(name string) string {
	if _, err := os.Stat(name); err == nil {
		return name
	}
	fallback := filepath.Join("examples", "dsl", name)
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	return name
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// First-class step logs: always Info to stderr so the pipeline is predictable. Use LOG_LEVEL=debug for more.
	logLevel := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

	agentsPath := envPath("AGENTS_FILE", "agents.json")
	workflowPath := envPath("WORKFLOW_FILE", "workflow.json")

	_, wf, compiled, agentNames, err := loadAndCompile(agentsPath, workflowPath)
	if err != nil {
		slog.Error("load/compile", "err", err)
		os.Exit(1)
	}

	llm, defaultModel := mustProvider(ctx)
	for _, cfg := range compiled.Agents {
		cfg.Model = defaultModel
	}
	sink := newReplSink(col)
	runner := dsl.NewRunner(compiled, llm, dsl.WithOutputSink(sink.Sink()))

	chatAgent := resolveChatAgent(wf, agentNames)
	if chatAgent == "" {
		fmt.Fprintln(os.Stderr, col(colRed, "no chat workflow or agent found in workflow.json"))
		os.Exit(1)
	}

	workflows := compiledWorkflows(wf, compiled)
	for _, cw := range workflows {
		cw.BindRunner(runner)
	}
	runner.BindWorkflows(workflows)

	stepLog("startup", "chat_agent", chatAgent)
	printBanner(chatAgent)
	session := "session-1"
	state := replState{sink: sink}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print(col(colBold+colGreen, "You  › "))
		if !scanner.Scan() {
			fmt.Println()
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		done, isCommand := runCommand(input, &state)
		if done {
			return
		}
		if isCommand {
			continue
		}
		if runUserMessage(ctx, input, chatAgent, session, runner, &state) {
			continue
		}
	}

	if err := scanner.Err(); err != nil {
		slog.Error("stdin", "err", err)
	}
	fmt.Printf(col(colDim, "\nSession total: %d in · %d out · %d tokens\n"),
		state.totalIn, state.totalOut, state.totalIn+state.totalOut)
}

func envPath(envKey, defaultName string) string {
	if p := os.Getenv(envKey); p != "" {
		return p
	}
	return resolveConfigPath(defaultName)
}

type replState struct {
	history  []niro.Message
	totalIn  int
	totalOut int
	sink     *replSink
}

// replSink prints LLM output (response, thinking, tool calls) to stdout via the output.Sink API.
type replSink struct {
	printedAI bool
	col       func(c, s string) string
}

func newReplSink(col func(c, s string) string) *replSink {
	return &replSink{col: col}
}

func (s *replSink) Reset() { s.printedAI = false }

func (s *replSink) Sink() *output.Sink {
	return &output.Sink{
		OnText: func(ctx context.Context, text string) error {
			if !s.printedAI {
				fmt.Print(s.col(colBold+colCyan, "AI   › "))
				s.printedAI = true
			}
			fmt.Print(text)
			return nil
		},
		OnThinking: func(ctx context.Context, text string) error {
			if !s.printedAI {
				fmt.Print(s.col(colBold+colCyan, "AI   › "))
				s.printedAI = true
			}
			fmt.Println()
			fmt.Print(s.col(colDim, "Think› "))
			fmt.Println(text)
			return nil
		},
		OnToolCall: func(ctx context.Context, call *niro.ToolCall) error {
			if !s.printedAI {
				fmt.Print(s.col(colBold+colCyan, "AI   › "))
				s.printedAI = true
			}
			fmt.Println()
			argsStr := string(call.Args)
			stepLog("tool_call", "name", call.Name, "args", argsStr)
			args := compactJSON(call.Args)
			if args != "" {
				args = "(" + args + ")"
			}
			fmt.Println(s.col(colYellow, "  🔧 "+call.Name+args))
			return nil
		},
		OnToolResult: func(ctx context.Context, res *niro.ToolResult) error {
			stepLog("tool_result", "call_id", res.CallID, "is_error", res.IsError, "content", truncateForLog(res.Content, 200))
			preview := truncateForLog(res.Content, 120)
			if res.IsError {
				fmt.Println(s.col(colRed, "  ← err: "+preview))
			} else {
				fmt.Println(s.col(colDim, "  ← "+preview))
			}
			return nil
		},
		OnEnd: func(ctx context.Context, u niro.Usage) error {
			stepLog("stream_end", "in", u.InputTokens, "out", u.OutputTokens, "total", u.TotalTokens)
			fmt.Println()
			return nil
		},
	}
}

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// runCommand handles /help, /clear, /quit, etc.
// Returns (done=true to exit main, isCommand=true to continue loop without running agent).
func runCommand(input string, state *replState) (done, isCommand bool) {
	switch strings.ToLower(input) {
	case "/quit", "/exit", "/q":
		fmt.Println(col(colDim, "Goodbye."))
		return true, true
	case "/help":
		printHelp()
		return false, true
	case "/clear":
		state.history = nil
		state.totalIn, state.totalOut = 0, 0
		fmt.Println(col(colDim, "Conversation cleared."))
		return false, true
	case "/history":
		printHistory(state.history)
		return false, true
	case "/usage":
		fmt.Printf(col(colDim, "Session: %d in · %d out · %d total tokens\n"),
			state.totalIn, state.totalOut, state.totalIn+state.totalOut)
		return false, true
	}
	return false, false
}

// reportErr logs the error as a step and with slog, then prints a short line to stderr.
// Skips when err is nil or context.Canceled. Adds "(retryable)" when niro.IsRetryable(err).
func reportErr(scope string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	stepLog("error", "scope", scope, "err", err)
	slog.Error("request failed", "scope", scope, "err", err)
	msg := err.Error()
	if niro.IsRetryable(err) {
		msg += " (retryable)"
	}
	fmt.Fprintln(os.Stderr, col(colRed, scope+": "+msg))
}

// runUserMessage runs the chat agent, drains the stream (sink prints output), runs handoff if requested, updates history and usage. Returns true on error.
func runUserMessage(ctx context.Context, input, chatAgent, session string, runner *dsl.Runner, state *replState) bool {
	stepLog("turn_start", "session", session, "agent", chatAgent, "input_len", len(input))

	state.history = append(state.history, niro.UserText(input))
	runCtx := dsl.NewRunContext()
	runCtx.Set("session", map[string]any{"id": session})
	runCtx.Set("messages", state.history)
	runCtx.Set("event", map[string]any{"text": input})

	stepLog("agent_start", "session", session, "agent", chatAgent)
	stream, err := runner.Stream(ctx, runCtx, chatAgent, session)
	if err != nil {
		stepLog("error", "session", session, "phase", "stream", "err", err)
		reportErr("stream", err)
		state.history = state.history[:len(state.history)-1]
		return true
	}

	state.sink.Reset()
	reply, handoffTarget := drainStream(ctx, stream)
	if err := stream.Err(); err != nil {
		stepLog("error", "session", session, "phase", "drain", "err", err)
		reportErr("stream", err)
		state.history = state.history[:len(state.history)-1]
		return true
	}

	u := stream.Usage()
	stepLog("agent_end", "session", session, "agent", chatAgent, "reply_len", len(reply), "handoff_target", handoffTarget, "in", u.InputTokens, "out", u.OutputTokens)

	if handoffTarget != "" {
		stepLog("handoff_start", "session", session, "target", handoffTarget)
		state.sink.Reset() // handoff agent output gets its own "AI   › " line
		handoffReply, handoffUsage, err := runner.RunHandoff(ctx, state.history, input, reply, handoffTarget, session)
		if err != nil {
			stepLog("handoff_end", "session", session, "target", handoffTarget, "err", err)
			reportErr("handoff", err)
		} else {
			stepLog("handoff_end", "session", session, "target", handoffTarget, "reply_len", len(handoffReply), "in", handoffUsage.InputTokens, "out", handoffUsage.OutputTokens)
			u.InputTokens += handoffUsage.InputTokens
			u.OutputTokens += handoffUsage.OutputTokens
			u.TotalTokens += handoffUsage.TotalTokens
			if handoffReply != "" {
				reply = handoffReply
			}
		}
	}

	if reply != "" {
		state.history = append(state.history, niro.AssistantText(reply))
	}
	state.totalIn += u.InputTokens
	state.totalOut += u.OutputTokens
	stepLog("turn_end", "session", session, "total_in", state.totalIn, "total_out", state.totalOut)
	if u.TotalTokens > 0 {
		fmt.Printf(col(colDim, "     ╌ %d in · %d out · %d total\n"), u.InputTokens, u.OutputTokens, u.TotalTokens)
	}
	return false
}

func loadAndCompile(agentsPath, workflowPath string) (*dsl.DSLDefinition, *dsl.WorkflowDefinition, *dsl.NiroDefinition, map[string]struct{}, error) {
	def, err := dsl.ParseDSLFile(agentsPath)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("agents: %w", err)
	}
	if err := dsl.Validate(def); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("validate agents: %w", err)
	}

	compiled, err := def.Compile(dsl.CompileOptions{})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("compile agents: %w", err)
	}

	agentNames := make(map[string]struct{})
	for name := range compiled.Agents {
		agentNames[name] = struct{}{}
	}

	wf, err := dsl.ParseWorkflowFile(workflowPath)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("workflow: %w", err)
	}
	if err := dsl.ValidateWorkflow(wf, agentNames); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("validate workflow: %w", err)
	}

	return def, wf, compiled, agentNames, nil
}

// ── Workflow & agent resolution ───────────────────────────────────────────────

func resolveChatAgent(wf *dsl.WorkflowDefinition, agentNames map[string]struct{}) string {
	if wf == nil {
		return ""
	}
	if node, ok := wf.Workflows["chat"]; ok {
		if node.Agent != "" {
			return node.Agent
		}
		if len(node.Agents) > 0 {
			return node.Agents[0]
		}
	}
	for name := range agentNames {
		return name
	}
	return ""
}

func compiledWorkflows(wf *dsl.WorkflowDefinition, nd *dsl.NiroDefinition) map[string]*dsl.CompiledWorkflow {
	if wf == nil || nd == nil {
		return nil
	}
	agentNames := make(map[string]struct{})
	for name := range nd.Agents {
		agentNames[name] = struct{}{}
	}
	out, _ := wf.CompileWorkflow(agentNames)
	return out
}

// ── Workflow execution ──────────────────────────────────────────────────────────

// drainStream consumes the stream (output is handled by the runner's output sink), accumulates reply text,
// and detects handoff from KindCustom "handoff" or from handoff tool args. Returns (reply, handoffTarget).
func drainStream(ctx context.Context, stream *niro.Stream) (reply string, handoffTarget string) {
	var sb strings.Builder
	for stream.Next(ctx) {
		f := stream.Frame()
		switch f.Kind {
		case niro.KindText:
			sb.WriteString(f.Text)
		case niro.KindToolCall:
			if f.Tool != nil && handoffTarget == "" && f.Tool.Name == "handoff" && len(f.Tool.Args) > 0 {
				var m map[string]any
				if json.Unmarshal(f.Tool.Args, &m) == nil {
					if t, _ := m["target"].(string); t != "" {
						handoffTarget = strings.TrimSpace(t)
					}
				}
			}
		case niro.KindCustom:
			if f.Custom != nil && f.Custom.Type == "handoff" && handoffTarget == "" {
				if s, ok := f.Custom.Data.(string); ok {
					handoffTarget = strings.TrimSpace(s)
				} else {
					handoffTarget = strings.TrimSpace(fmt.Sprint(f.Custom.Data))
				}
			}
		}
	}
	fmt.Println()
	return sb.String(), handoffTarget
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if niro.JSONUnmarshal(raw, &m) != nil {
		s := string(raw)
		if len(s) > 60 {
			s = s[:57] + "…"
		}
		return s
	}
	if len(m) == 0 {
		return ""
	}
	if len(m) == 1 {
		for _, v := range m {
			b, _ := niro.JSONMarshal(v)
			s := strings.Trim(string(b), `"`)
			if len(s) > 60 {
				s = s[:57] + "…"
			}
			return s
		}
	}
	b, _ := niro.JSONMarshal(m)
	s := string(b)
	if len(s) > 80 {
		s = s[:77] + "…"
	}
	return s
}

// ── UI ────────────────────────────────────────────────────────────────────────

func printBanner(agent string) {
	title := "DSL Chat  —  " + agent
	w := len(title) + 4
	line := strings.Repeat("─", w)
	fmt.Println(col(colBold+colCyan, "┌"+line+"┐"))
	fmt.Println(col(colBold+colCyan, "│  "+title+"  │"))
	fmt.Println(col(colBold+colCyan, "└"+line+"┘"))
	fmt.Println(col(colDim, "Tools from agents.json (e.g. lookup_cep). Type /help for commands."))
	fmt.Println()
}

func printHelp() {
	fmt.Println(col(colBold, "Commands:"))
	for _, c := range [][2]string{
		{"/help", "show this message"},
		{"/clear", "reset conversation"},
		{"/history", "print conversation"},
		{"/usage", "token usage"},
		{"/quit", "exit"},
	} {
		fmt.Printf("  %-12s  %s\n", col(colYellow, c[0]), col(colDim, c[1]))
	}
	fmt.Println()
}

func printHistory(history []niro.Message) {
	if len(history) == 0 {
		fmt.Println(col(colDim, "(no messages yet)"))
		return
	}
	fmt.Println(col(colBold, "── Conversation ───────────────────────────"))
	for i, m := range history {
		label := col(colGreen, "User ")
		if m.Role == niro.RoleAssistant {
			label = col(colCyan, "AI   ")
		}
		var sb strings.Builder
		for _, p := range m.Parts {
			if p.Kind == niro.KindText {
				sb.WriteString(p.Text)
			}
		}
		preview := sb.String()
		if len(preview) > 120 {
			preview = preview[:117] + "…"
		}
		fmt.Printf("  %2d. %s %s\n", i+1, label, col(colDim, preview))
	}
	fmt.Println(col(colBold, "───────────────────────────────────────────"))
}

// ── Provider factory ──────────────────────────────────────────────────────────

func mustProvider(ctx context.Context) (niro.Provider, string) {
	providerName := strings.ToLower(strings.TrimSpace(os.Getenv("PROVIDER")))
	if providerName == "" {
		providerName = "openai"
	}
	modelName := strings.TrimSpace(os.Getenv("MODEL"))

	switch providerName {
	case "gemini":
		key := os.Getenv("GEMINI_API_KEY")
		if key == "" {
			reportErr("provider", niro.NewError(niro.ErrCodeAuthenticationFailed, "GEMINI_API_KEY is not set"))
			os.Exit(1)
		}
		if modelName == "" {
			modelName = "gemini-2.5-flash"
		}
		p, err := google.New(key, google.WithModel(modelName))
		if err != nil {
			reportErr("provider", err)
			os.Exit(1)
		}
		return p, modelName

	case "bedrock":
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			reportErr("provider", err)
			os.Exit(1)
		}
		if modelName == "" {
			modelName = "anthropic.claude-3-5-haiku-20241022-v1:0"
		}
		return bedrock.New(cfg, bedrock.WithModel(modelName)), modelName

	case "ollama":
		baseURL := os.Getenv("OLLAMA_BASE_URL")
		if baseURL == "" {
			baseURL = "http://localhost:11434/v1"
		}
		if modelName == "" {
			modelName = "llama3.2"
		}
		return compat.New(baseURL, "", compat.WithModel(modelName)), modelName

	default:
		fmt.Fprintf(os.Stderr, col(colRed, "unknown PROVIDER=%q — openai|anthropic|gemini|bedrock|ollama\n"), os.Getenv("PROVIDER"))
		os.Exit(1)
	}
	return nil, ""
}
