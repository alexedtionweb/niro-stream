// Command chat is an interactive multi-turn terminal chat with any supported
// provider. It demonstrates streaming output, tool calling, and conversation
// history across turns.
//
// Built-in tools available to the model:
//
// get_current_time(timezone?)  — real clock, IANA timezone name
// calculator(expression)       — arithmetic + math functions (sqrt, sin, …)
// web_fetch(url)               — HTTP GET, returns plain-text excerpt
//
// Select a provider with the PROVIDER env var (default: openai):
//
// PROVIDER=openai    OPENAI_API_KEY=sk-...        go run ./chat
// PROVIDER=anthropic ANTHROPIC_API_KEY=sk-ant-... go run ./chat
// PROVIDER=gemini    GEMINI_API_KEY=...            go run ./chat
// PROVIDER=bedrock                                 go run ./chat
// PROVIDER=ollama    MODEL=llama3.2               go run ./chat
//
// Commands (type while chatting):
//
// /help     — show this list
// /clear    — start a new conversation
// /history  — print full conversation so far
// /usage    — show session token usage
// /quit     — exit (Ctrl-D works too)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/provider/anthropic"
	"github.com/alexedtionweb/niro-stream/provider/bedrock"
	"github.com/alexedtionweb/niro-stream/provider/compat"
	"github.com/alexedtionweb/niro-stream/provider/google"
	"github.com/alexedtionweb/niro-stream/provider/openai"
	"github.com/alexedtionweb/niro-stream/tools"
	"github.com/aws/aws-sdk-go-v2/config"
)

// ── ANSI colour helpers ───────────────────────────────────────────────────────

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

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	level := slog.LevelWarn // keep the terminal clean; use LOG_LEVEL=debug for details
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	llm, modelName, providerName := mustProvider(ctx)
	ts := buildToolset()
	loop := tools.NewToolLoop(ts, 8)

	systemPrompt := fmt.Sprintf(
		"You are a helpful assistant with access to tools. "+
			"Today is %s. "+
			"Be concise but thorough. "+
			"Use tools whenever they would produce a more accurate or up-to-date answer.",
		time.Now().Format("Monday, 2 January 2006"),
	)

	printBanner(providerName, modelName)

	var (
		history  []niro.Message
		totalIn  int
		totalOut int
	)

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print(col(colBold+colGreen, "You  › "))

		if !scanner.Scan() {
			fmt.Println()
			break // EOF — Ctrl-D
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		// ── Built-in REPL commands ────────────────────────────────────────
		switch strings.ToLower(input) {
		case "/quit", "/exit", "/q":
			fmt.Println(col(colDim, "Goodbye."))
			return
		case "/help":
			printHelp()
			continue
		case "/clear":
			history = nil
			totalIn, totalOut = 0, 0
			fmt.Println(col(colDim, "Conversation cleared."))
			continue
		case "/history":
			printHistory(history)
			continue
		case "/usage":
			fmt.Printf(col(colDim, "Session: %d in · %d out · %d total tokens\n"),
				totalIn, totalOut, totalIn+totalOut)
			continue
		}

		// ── Build request ─────────────────────────────────────────────────
		history = append(history, niro.UserText(input))

		req := ts.Apply(&niro.Request{
			Model:        modelName,
			SystemPrompt: systemPrompt,
			Messages:     history,
			Options: niro.Options{MaxTokens: 1024, Temperature: niro.Temp(0.7),
				Cache: &niro.CacheOptions{
					Scope: niro.CacheScopePrefix,
					Mode:  niro.CacheAuto,
					TTL:   time.Hour,
				}},
		})

		stream, err := loop.GenerateWithTools(ctx, llm, req)
		if err != nil {
			fmt.Fprintln(os.Stderr, col(colRed, "Error: "+err.Error()))
			history = history[:len(history)-1]
			continue
		}

		// ── Stream the response ───────────────────────────────────────────
		reply := streamResponse(ctx, stream)

		if err := stream.Err(); err != nil {
			if !errors.Is(err, context.Canceled) {
				fmt.Fprintln(os.Stderr, col(colRed, "\nStream error: "+err.Error()))
			}
			history = history[:len(history)-1]
			continue
		}

		if reply != "" {
			history = append(history, niro.AssistantText(reply))
		}

		u := stream.Usage()
		totalIn += u.InputTokens
		totalOut += u.OutputTokens
		if u.TotalTokens > 0 {
			fmt.Printf(col(colDim, "     ╌ %d in · %d out · %d total\n"),
				u.InputTokens, u.OutputTokens, u.TotalTokens)
		}
	}

	if err := scanner.Err(); err != nil {
		slog.Error("stdin", "err", err)
	}
	fmt.Printf(col(colDim, "\nSession total: %d in · %d out · %d tokens\n"),
		totalIn, totalOut, totalIn+totalOut)
}

// streamResponse drains the stream, printing text tokens and tool-call
// notifications inline. Returns the full assistant text for history.
func streamResponse(ctx context.Context, stream *niro.Stream) string {
	var (
		reply       strings.Builder
		printedAI   bool
		atLineStart = true
	)

	ensureAI := func() {
		if !printedAI {
			fmt.Print(col(colBold+colCyan, "AI   › "))
			printedAI = true
			atLineStart = false
		}
	}

	for stream.Next(ctx) {
		f := stream.Frame()
		switch f.Kind {
		case niro.KindText:
			ensureAI()
			fmt.Print(f.Text)
			reply.WriteString(f.Text)
			atLineStart = strings.HasSuffix(f.Text, "\n")

		case niro.KindToolCall:
			if f.Tool == nil {
				continue
			}
			if !atLineStart {
				fmt.Println()
				atLineStart = true
			}
			indent := "       " // aligns with text after "AI   › "
			if !printedAI {
				// First frame is a tool call — print the AI prefix here.
				indent = col(colBold+colCyan, "AI   › ")
				printedAI = true
			}
			args := compactJSON(f.Tool.Args)
			if args != "" {
				args = "(" + args + ")"
			}
			fmt.Printf("%s%s\n", indent, col(colYellow, "🔧 "+f.Tool.Name+args))
			atLineStart = true
		}
	}

	if !atLineStart {
		fmt.Println()
	}
	return reply.String()
}

// ── UI helpers ────────────────────────────────────────────────────────────────

func printBanner(provider, model string) {
	title := "Chat  —  " + provider
	if model != "" {
		title += " / " + model
	}
	w := len(title) + 4
	line := strings.Repeat("─", w)
	fmt.Println(col(colBold+colCyan, "┌"+line+"┐"))
	fmt.Println(col(colBold+colCyan, "│  "+title+"  │"))
	fmt.Println(col(colBold+colCyan, "└"+line+"┘"))
	fmt.Println(col(colDim, "Tools: get_current_time · calculator · web_fetch"))
	fmt.Println(col(colDim, "Type /help for commands. Ctrl-D or /quit to exit."))
	fmt.Println()
}

func printHelp() {
	fmt.Println(col(colBold, "Commands:"))
	for _, c := range [][2]string{
		{"/help", "show this message"},
		{"/clear", "reset conversation history"},
		{"/history", "print conversation so far"},
		{"/usage", "show token usage for this session"},
		{"/quit", "exit  (Ctrl-D also works)"},
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
	fmt.Println(col(colBold, "── Conversation history ────────────────────"))
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
	fmt.Println(col(colBold, "────────────────────────────────────────────"))
}

// compactJSON produces a short human-readable summary of a JSON object.
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
	// Single-value shorthand: show just the value, not the key.
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

// ── Tool implementations ──────────────────────────────────────────────────────

func buildToolset() *tools.Toolset {
	ts := tools.NewToolset()
	registerTimeTool(ts)
	registerCalculatorTool(ts)
	registerWebFetchTool(ts)
	return ts
}

func registerTimeTool(ts *tools.Toolset) {
	def, _ := tools.NewToolDefinitionAny(
		"get_current_time",
		"Return the current date and time. Optionally specify an IANA timezone "+
			"(e.g. 'America/New_York', 'Asia/Tokyo', 'Europe/London'). Defaults to local time.",
		json.RawMessage(`{"type":"object","properties":{"timezone":{"type":"string","description":"IANA timezone name, e.g. America/New_York or Asia/Tokyo. Defaults to local time."}}}`),
		func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct{ Timezone string `json:"timezone"` }
			_ = niro.JSONUnmarshal(args, &a)
			loc := time.Local
			if a.Timezone != "" {
				l, err := time.LoadLocation(a.Timezone)
				if err != nil {
					return nil, fmt.Errorf("unknown timezone %q", a.Timezone)
				}
				loc = l
			}
			now := time.Now().In(loc)
			return map[string]string{
				"datetime": now.Format("2006-01-02 15:04:05 MST"),
				"date":     now.Format("Monday, 2 January 2006"),
				"time":     now.Format("15:04:05"),
				"timezone": now.Format("MST (UTC-07:00)"),
				"unix":     strconv.FormatInt(now.Unix(), 10),
			}, nil
		},
	)
	ts.MustRegister(def)
}

func registerCalculatorTool(ts *tools.Toolset) {
	def, _ := tools.NewToolDefinitionAny(
		"calculator",
		"Evaluate a mathematical expression. "+
			"Supports +, -, *, /, ^ (power), parentheses, and the functions: "+
			"sqrt, abs, sin, cos, tan, asin, acos, atan, log (natural), log2, log10, exp, ceil, floor, round, cbrt. "+
			"Constants: pi, e. Examples: 'sqrt(2) * pi', '2^10', 'sin(pi/6)'.",
		json.RawMessage(`{"type":"object","properties":{"expression":{"type":"string","description":"Mathematical expression to evaluate, e.g. sqrt(2)*pi or 2^10"}},"required":["expression"]}`),
		func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct{ Expression string `json:"expression"` }
			if err := niro.JSONUnmarshal(args, &a); err != nil || a.Expression == "" {
				return nil, errors.New("expression is required")
			}
			result, err := evalExpr(a.Expression)
			if err != nil {
				return nil, err
			}
			var formatted string
			if result == math.Trunc(result) && !math.IsInf(result, 0) && !math.IsNaN(result) {
				formatted = strconv.FormatInt(int64(result), 10)
			} else {
				formatted = strconv.FormatFloat(result, 'g', 12, 64)
			}
			return map[string]string{"expression": a.Expression, "result": formatted}, nil
		},
	)
	ts.MustRegister(def)
}

func registerWebFetchTool(ts *tools.Toolset) {
	def, _ := tools.NewToolDefinitionAny(
		"web_fetch",
		"Fetch a public URL and return its text content (HTML tags stripped). "+
			"Useful for reading documentation, GitHub READMEs, Wikipedia articles, or any public page. "+
			"Returns up to 3000 characters of content.",
		json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"The fully-qualified public URL to fetch, e.g. https://example.com"}},"required":["url"]}`),
		func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct{ URL string `json:"url"` }
			if err := niro.JSONUnmarshal(args, &a); err != nil || a.URL == "" {
				return nil, errors.New("url is required")
			}
			fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, a.URL, nil)
			if err != nil {
				return nil, fmt.Errorf("invalid URL: %w", err)
			}
			req.Header.Set("User-Agent", "niro-chat/1.0 (demo)")
			req.Header.Set("Accept", "text/html,text/plain,*/*")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, fmt.Errorf("fetch failed: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
			if err != nil {
				return nil, fmt.Errorf("read body: %w", err)
			}
			text := stripHTML(string(body))
			if len(text) > 3000 {
				text = text[:2997] + "…"
			}
			return map[string]any{"url": a.URL, "status": resp.StatusCode, "chars": len(text), "text": text}, nil
		},
	)
	ts.MustRegister(def)
}

// stripHTML removes HTML/XML tags and collapses whitespace.
func stripHTML(s string) string {
	var b strings.Builder
	b.Grow(len(s) / 2)
	inTag := false
	prevSpace := true
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		case !inTag:
			if unicode.IsSpace(r) {
				if !prevSpace {
					b.WriteByte(' ')
					prevSpace = true
				}
			} else {
				b.WriteRune(r)
				prevSpace = false
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// ── Expression evaluator ──────────────────────────────────────────────────────
//
// Grammar (operator precedence, lowest → highest):
//   expr    = add
//   add     = mul  ( ('+'|'-') mul  )*
//   mul     = power( ('*'|'/') power)*
//   power   = unary ( '^' unary )*     right-associative
//   unary   = '-' unary | primary
//   primary = NUMBER | IDENT '(' expr ')' | '(' expr ')' | IDENT

type exprParser struct {
	s   string
	pos int
}

func evalExpr(expr string) (float64, error) {
	p := &exprParser{s: strings.TrimSpace(expr)}
	v, err := p.parseAdd()
	if err != nil {
		return 0, err
	}
	p.skipWS()
	if p.pos < len(p.s) {
		return 0, fmt.Errorf("unexpected %q at position %d", string(p.s[p.pos]), p.pos)
	}
	return v, nil
}

func (p *exprParser) skipWS() {
	for p.pos < len(p.s) && p.s[p.pos] == ' ' {
		p.pos++
	}
}

func (p *exprParser) peek() byte {
	p.skipWS()
	if p.pos >= len(p.s) {
		return 0
	}
	return p.s[p.pos]
}

func (p *exprParser) next() byte {
	p.skipWS()
	if p.pos >= len(p.s) {
		return 0
	}
	c := p.s[p.pos]
	p.pos++
	return c
}

func (p *exprParser) parseAdd() (float64, error) {
	l, err := p.parseMul()
	if err != nil {
		return 0, err
	}
	for {
		op := p.peek()
		if op != '+' && op != '-' {
			break
		}
		p.next()
		r, err := p.parseMul()
		if err != nil {
			return 0, err
		}
		if op == '+' {
			l += r
		} else {
			l -= r
		}
	}
	return l, nil
}

func (p *exprParser) parseMul() (float64, error) {
	l, err := p.parsePow()
	if err != nil {
		return 0, err
	}
	for {
		op := p.peek()
		if op != '*' && op != '/' {
			break
		}
		p.next()
		r, err := p.parsePow()
		if err != nil {
			return 0, err
		}
		if op == '*' {
			l *= r
		} else {
			if r == 0 {
				return 0, errors.New("division by zero")
			}
			l /= r
		}
	}
	return l, nil
}

func (p *exprParser) parsePow() (float64, error) {
	base, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	if p.peek() == '^' {
		p.next()
		exp, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		return math.Pow(base, exp), nil
	}
	return base, nil
}

func (p *exprParser) parseUnary() (float64, error) {
	switch p.peek() {
	case '-':
		p.next()
		v, err := p.parseUnary()
		return -v, err
	case '+':
		p.next()
	}
	return p.parsePrimary()
}

func (p *exprParser) parsePrimary() (float64, error) {
	p.skipWS()
	if p.pos >= len(p.s) {
		return 0, errors.New("unexpected end of expression")
	}

	// Parenthesised sub-expression.
	if p.s[p.pos] == '(' {
		p.pos++
		v, err := p.parseAdd()
		if err != nil {
			return 0, err
		}
		p.skipWS()
		if p.pos >= len(p.s) || p.s[p.pos] != ')' {
			return 0, errors.New("missing closing ')'")
		}
		p.pos++
		return v, nil
	}

	// Number literal.
	if c := p.s[p.pos]; c >= '0' && c <= '9' || c == '.' {
		start := p.pos
		for p.pos < len(p.s) {
			c := p.s[p.pos]
			if c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' {
				p.pos++
			} else if (c == '-' || c == '+') && p.pos > start && (p.s[p.pos-1] == 'e' || p.s[p.pos-1] == 'E') {
				p.pos++
			} else {
				break
			}
		}
		return strconv.ParseFloat(p.s[start:p.pos], 64)
	}

	// Identifier: function or constant.
	if isAlpha(p.s[p.pos]) {
		start := p.pos
		for p.pos < len(p.s) && (isAlpha(p.s[p.pos]) || p.s[p.pos] >= '0' && p.s[p.pos] <= '9') {
			p.pos++
		}
		name := strings.ToLower(p.s[start:p.pos])
		p.skipWS()
		if p.pos < len(p.s) && p.s[p.pos] == '(' {
			p.pos++
			arg, err := p.parseAdd()
			if err != nil {
				return 0, err
			}
			p.skipWS()
			if p.pos >= len(p.s) || p.s[p.pos] != ')' {
				return 0, fmt.Errorf("missing ')' after %s(", name)
			}
			p.pos++
			return mathFunc(name, arg)
		}
		switch name {
		case "pi":
			return math.Pi, nil
		case "e":
			return math.E, nil
		case "phi":
			return math.Phi, nil
		case "inf", "infinity":
			return math.Inf(1), nil
		}
		return 0, fmt.Errorf("unknown identifier %q", name)
	}

	return 0, fmt.Errorf("unexpected character %q", string(p.s[p.pos]))
}

func mathFunc(name string, x float64) (float64, error) {
	switch name {
	case "sqrt":
		return math.Sqrt(x), nil
	case "cbrt":
		return math.Cbrt(x), nil
	case "abs":
		return math.Abs(x), nil
	case "sin":
		return math.Sin(x), nil
	case "cos":
		return math.Cos(x), nil
	case "tan":
		return math.Tan(x), nil
	case "asin":
		return math.Asin(x), nil
	case "acos":
		return math.Acos(x), nil
	case "atan":
		return math.Atan(x), nil
	case "log", "ln":
		return math.Log(x), nil
	case "log2":
		return math.Log2(x), nil
	case "log10":
		return math.Log10(x), nil
	case "exp":
		return math.Exp(x), nil
	case "ceil":
		return math.Ceil(x), nil
	case "floor":
		return math.Floor(x), nil
	case "round":
		return math.Round(x), nil
	case "sign":
		if x < 0 {
			return -1, nil
		} else if x > 0 {
			return 1, nil
		}
		return 0, nil
	case "fact", "factorial":
		if x < 0 || x != math.Trunc(x) {
			return 0, errors.New("factorial requires a non-negative integer")
		}
		r := 1.0
		for i := 2.0; i <= x; i++ {
			r *= i
		}
		return r, nil
	}
	return 0, fmt.Errorf("unknown function %q", name)
}

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

// ── Provider factory ──────────────────────────────────────────────────────────

func mustProvider(ctx context.Context) (niro.Provider, string, string) {
	providerName := strings.ToLower(strings.TrimSpace(os.Getenv("PROVIDER")))
	if providerName == "" {
		providerName = "openai"
	}
	modelName := strings.TrimSpace(os.Getenv("MODEL"))

	switch providerName {
	case "", "openai":
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" {
			fmt.Fprintln(os.Stderr, col(colRed, "error: OPENAI_API_KEY is not set"))
			os.Exit(1)
		}
		if modelName == "" {
			modelName = "gpt-4o"
		}
		return openai.New(key, openai.WithModel(modelName)), modelName, providerName

	case "anthropic":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			fmt.Fprintln(os.Stderr, col(colRed, "error: ANTHROPIC_API_KEY is not set"))
			os.Exit(1)
		}
		if modelName == "" {
			modelName = "claude-sonnet-4-5"
		}
		return anthropic.New(key, anthropic.WithModel(modelName)), modelName, providerName

	case "gemini":
		key := os.Getenv("GEMINI_API_KEY")
		if key == "" {
			fmt.Fprintln(os.Stderr, col(colRed, "error: GEMINI_API_KEY is not set"))
			os.Exit(1)
		}
		if modelName == "" {
			modelName = "gemini-2.0-flash"
		}
		p, err := google.New(key, google.WithModel(modelName))
		if err != nil {
			fmt.Fprintln(os.Stderr, col(colRed, "google: "+err.Error()))
			os.Exit(1)
		}
		return p, modelName, providerName

	case "bedrock":
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, col(colRed, "aws config: "+err.Error()))
			os.Exit(1)
		}
		if modelName == "" {
			modelName = "anthropic.claude-3-5-sonnet-20241022-v2:0"
		}
		return bedrock.New(cfg, bedrock.WithModel(modelName)), modelName, providerName

	case "ollama":
		baseURL := os.Getenv("OLLAMA_BASE_URL")
		if baseURL == "" {
			baseURL = "http://localhost:11434/v1"
		}
		if modelName == "" {
			modelName = "llama3.2"
		}
		return compat.New(baseURL, "", compat.WithModel(modelName)), modelName, providerName

	default:
		fmt.Fprintf(os.Stderr, col(colRed, "unknown PROVIDER=%q — valid: openai|anthropic|gemini|bedrock|ollama\n"),
			os.Getenv("PROVIDER"))
		os.Exit(1)
		return nil, "", ""
	}
}
