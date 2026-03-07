package tools

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

// HandoffSignal is returned by a tool handler to signal handoff to another agent or workflow.
// Handoff is a tool like any other: the model calls the handoff tool with a target; the handler
// returns HandoffSignal{Target: "name"}; the tool loop emits a KindCustom handoff frame and
// exits without adding a tool result. The runner (e.g. DSL RunHandoff) then runs the target.
// Use this instead of encoding handoff in result content.
type HandoffSignal struct {
	Target string // Agent or workflow name to hand off to
}

func (HandoffSignal) Error() string { return "handoff" }

// ToolExecutor defines how to execute a tool call.
type ToolExecutor interface {
	// Execute runs the named tool with the given arguments and returns the result.
	// The executor should handle errors appropriately (e.g. tool not found, execution failure)
	Execute(ctx context.Context, name string, args json.RawMessage) (string, error)
}

// ToolExecutorFunc adapts a function to ToolExecutor interface.
type ToolExecutorFunc func(ctx context.Context, name string, args json.RawMessage) (string, error)

func (f ToolExecutorFunc) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	return f(ctx, name, args)
}

// ToolLoop manages automatic tool calling loops.
// It reads tool call frames from a stream, executes them, and returns a new stream
// with the results integrated.
type ToolLoop struct {
	executor ToolExecutor
	opts     ToolStreamOptions
}

// ToolStreamOptions controls automatic tool execution behavior.
type ToolStreamOptions struct {
	MaxRounds       int
	Parallel        bool
	EmitToolResults bool
	ToolTimeout     time.Duration
	StreamBuffer    int
	// Approver is an optional human-in-the-loop gate.
	// When set, every tool call is passed to Approver.Approve before execution.
	// A nil Approver means all calls proceed without review.
	Approver ToolApprover
}

// DefaultToolStreamOptions returns low-latency defaults.
func DefaultToolStreamOptions() ToolStreamOptions {
	return ToolStreamOptions{
		MaxRounds:       8,
		Parallel:        true,
		EmitToolResults: true,
		ToolTimeout:     30 * time.Second,
		StreamBuffer:    16,
	}
}

// NewToolLoop creates a ToolLoop with defaults + maxRounds override.
func NewToolLoop(executor ToolExecutor, maxRounds int) *ToolLoop {
	opts := DefaultToolStreamOptions()
	if maxRounds > 0 {
		opts.MaxRounds = maxRounds
	}
	return &ToolLoop{executor: executor, opts: opts}
}

// NewToolLoopWithOptions creates a ToolLoop with explicit options.
func NewToolLoopWithOptions(executor ToolExecutor, opts ToolStreamOptions) *ToolLoop {
	d := DefaultToolStreamOptions()
	if opts.MaxRounds > 0 {
		d.MaxRounds = opts.MaxRounds
	}
	d.Parallel = opts.Parallel
	d.EmitToolResults = opts.EmitToolResults
	if opts.ToolTimeout > 0 {
		d.ToolTimeout = opts.ToolTimeout
	}
	if opts.StreamBuffer > 0 {
		d.StreamBuffer = opts.StreamBuffer
	}
	d.Approver = opts.Approver
	return &ToolLoop{executor: executor, opts: d}
}

// GenerateWithTools executes tool loops and preserves streaming output.
func (tl *ToolLoop) GenerateWithTools(ctx context.Context, provider niro.Provider, req *niro.Request) (*niro.Stream, error) {
	if tl == nil || tl.executor == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "tool executor is required")
	}
	if req == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "request is nil")
	}

	buf := tl.opts.StreamBuffer
	if buf <= 0 {
		buf = 16
	}
	out, em := niro.NewStream(buf)
	go func() {
		defer em.Close()
		if err := tl.run(ctx, provider, req, em); err != nil {
			em.Error(err)
		}
	}()
	return out, nil
}

func (tl *ToolLoop) run(ctx context.Context, provider niro.Provider, req *niro.Request, out *niro.Emitter) error {
	cur := *req
	messages := append([]niro.Message(nil), req.Messages...)

	var lastResp *niro.ResponseMeta
	for round := 0; round < tl.opts.MaxRounds; round++ {
		cur.Messages = messages

		// After the first round relax ToolChoice so the model can produce a
		// final text response instead of being forced into another tool call.
		// ToolChoiceRequired and func:name choices only make sense for round 0.
		if round > 0 {
			tc := cur.ToolChoice
			if tc == niro.ToolChoiceRequired || (len(tc) > 5 && tc[:5] == "func:") {
				cur.ToolChoice = niro.ToolChoiceAuto
			}
		}

		stream, err := provider.Generate(ctx, &cur)
		if err != nil {
			return err
		}

		assistantText := make([]byte, 0, 512)
		toolCalls := make([]niro.ToolCall, 0, 2)

		// Parallel streaming dispatch: fire a goroutine for each tool call the
		// moment its KindToolCall frame arrives — before the stream finishes.
		// This overlaps tool execution with the tail of the LLM response,
		// eliminating the "full drain then execute" latency penalty.
		//
		// Provider note: OpenAI and Bedrock emit KindToolCall mid-stream so
		// goroutines fire early. Anthropic emits all tool calls after the last
		// text token (SDK accumulator limitation), so the overlap is smaller
		// but the goroutines still start before the stream channel closes.
		var resultChans []chan niro.ToolResult

		for stream.Next(ctx) {
			f := stream.Frame()
			if f.Kind == niro.KindText {
				assistantText = append(assistantText, f.Text...)
			}
			if f.Kind == niro.KindToolCall && f.Tool != nil {
				call := *f.Tool
				toolCalls = append(toolCalls, call)
				if tl.opts.Parallel {
					ch := make(chan niro.ToolResult, 1)
					resultChans = append(resultChans, ch)
					go func(c niro.ToolCall) { ch <- tl.execOne(ctx, c) }(call)
				}
			}
			if err := out.Emit(ctx, f); err != nil {
				return err
			}
		}
		if err := stream.Err(); err != nil {
			return err
		}
		lastResp = stream.Response()

		// Re-emit the inner stream's usage to the outer stream so callers of
		// GenerateWithTools see correct token counts via stream.Usage().
		// KindUsage frames are silently consumed by stream.Next() and never
		// forwarded to out.Emit, so we must re-emit them explicitly here.
		if u := stream.Usage(); u.InputTokens > 0 || u.OutputTokens > 0 || u.TotalTokens > 0 {
			uCopy := u
			if err := out.Emit(ctx, niro.Frame{Kind: niro.KindUsage, Usage: &uCopy}); err != nil {
				return err
			}
		}

		assistantMsg := assistantFromRound(string(assistantText), toolCalls)
		if len(assistantMsg.Parts) > 0 {
			messages = append(messages, assistantMsg)
		}

		if len(toolCalls) == 0 {
			if lastResp != nil {
				out.SetResponse(lastResp)
			}
			return nil
		}

		// Collect tool results. Parallel mode drains the pre-fired channels;
		// a ctx.Done() case here lets us abort immediately if the caller
		// hangs up (e.g. barge-in on a voice call) without waiting for tools.
		var results []niro.ToolResult
		if tl.opts.Parallel && len(resultChans) > 0 {
			results = make([]niro.ToolResult, len(resultChans))
			for i, ch := range resultChans {
				select {
				case r := <-ch:
					results[i] = r
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		} else {
			results = tl.executeTools(ctx, toolCalls)
		}

		// If any tool returned a handoff signal (HandoffTarget set), emit a KindCustom
		// handoff frame and exit the loop without adding tool results.
		for i := range results {
			tr := results[i]
			if tr.HandoffTarget != "" {
				if err := out.Emit(ctx, niro.Frame{
					Kind:   niro.KindCustom,
					Custom: &niro.ExperimentalFrame{Type: "handoff", Data: tr.HandoffTarget},
				}); err != nil {
					return err
				}
				return nil
			}
		}

		// Group all tool results into a single RoleTool message.
		// Anthropic requires every result for a single assistant turn to appear
		// in one user-turn message. OpenAI and Bedrock providers split
		// multi-part messages during encoding so they are unaffected.
		toolMsgParts := make([]niro.Part, 0, len(results))
		for i := range results {
			tr := results[i]
			toolMsgParts = append(toolMsgParts, niro.Part{
				Kind: niro.KindToolResult,
				Result: &niro.ToolResult{
					CallID:  tr.CallID,
					Content: tr.Content,
					IsError: tr.IsError,
				},
			})
			if tl.opts.EmitToolResults {
				res := tr
				if err := out.Emit(ctx, niro.Frame{Kind: niro.KindToolResult, Result: &res}); err != nil {
					return err
				}
			}
		}
		messages = append(messages, niro.Message{Role: niro.RoleTool, Parts: toolMsgParts})
	}

	return niro.NewErrorf(niro.ErrCodeStreamError, "tool loop exceeded max rounds (%d)", tl.opts.MaxRounds)
}

func assistantFromRound(text string, calls []niro.ToolCall) niro.Message {
	parts := make([]niro.Part, 0, 1+len(calls))
	if text != "" {
		parts = append(parts, niro.Part{Kind: niro.KindText, Text: text})
	}
	for i := range calls {
		call := calls[i]
		parts = append(parts, niro.Part{Kind: niro.KindToolCall, Tool: &call})
	}
	return niro.Message{Role: niro.RoleAssistant, Parts: parts}
}

// executeTools runs calls serially (the parallel path fires goroutines in run()).
// It bails early on context cancellation so a cancelled caller (e.g. voice
// barge-in) doesn't burn resources on tools that will be discarded.
func (tl *ToolLoop) executeTools(ctx context.Context, calls []niro.ToolCall) []niro.ToolResult {
	results := make([]niro.ToolResult, len(calls))
	for i := range calls {
		if err := ctx.Err(); err != nil {
			for j := i; j < len(calls); j++ {
				results[j] = niro.ToolResult{
					CallID:  calls[j].ID,
					Content: err.Error(),
					IsError: true,
				}
			}
			return results
		}
		results[i] = tl.execOne(ctx, calls[i])
	}
	return results
}

func (tl *ToolLoop) execOne(ctx context.Context, call niro.ToolCall) niro.ToolResult {
	// HITL: approval gate runs outside the tool timeout so the human has the
	// full outer context deadline to respond, not the tool execution window.
	if tl.opts.Approver != nil {
		decision, err := tl.opts.Approver.Approve(ctx, call)
		if err != nil {
			return niro.ToolResult{CallID: call.ID, Content: "approval error: " + err.Error(), IsError: true}
		}
		if !decision.Approved {
			reason := decision.Reason
			if reason == "" {
				reason = "tool call was not approved"
			}
			return niro.ToolResult{CallID: call.ID, Content: reason, IsError: true}
		}
	}

	execCtx := ctx
	cancel := func() {}
	if tl.opts.ToolTimeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, tl.opts.ToolTimeout)
	}
	defer cancel()

	content, err := tl.executor.Execute(execCtx, call.Name, call.Args)
	if err != nil {
		var h HandoffSignal
		if errors.As(err, &h) {
			return niro.ToolResult{CallID: call.ID, HandoffTarget: h.Target}
		}
		return niro.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true}
	}
	return niro.ToolResult{CallID: call.ID, Content: content}
}

// StreamWithToolHandling wraps a Provider and executes tool calls automatically.
type StreamWithToolHandling struct {
	provider niro.Provider
	loop     *ToolLoop
}

// NewStreamWithToolHandling creates a provider that automatically executes tools.
func NewStreamWithToolHandling(p niro.Provider, executor ToolExecutor, maxRounds int) *StreamWithToolHandling {
	return &StreamWithToolHandling{
		provider: p,
		loop:     NewToolLoop(executor, maxRounds),
	}
}

// NewStreamWithToolHandlingOptions creates a provider wrapper with full options.
func NewStreamWithToolHandlingOptions(p niro.Provider, executor ToolExecutor, opts ToolStreamOptions) *StreamWithToolHandling {
	return &StreamWithToolHandling{
		provider: p,
		loop:     NewToolLoopWithOptions(executor, opts),
	}
}

// Generate implements niro.Provider and preserves streaming while handling tools.
func (swth *StreamWithToolHandling) Generate(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
	return swth.loop.GenerateWithTools(ctx, swth.provider, req)
}
