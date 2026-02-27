package tools

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"ryn.dev/ryn"
)

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
	return &ToolLoop{executor: executor, opts: d}
}

// GenerateWithTools executes tool loops and preserves streaming output.
func (tl *ToolLoop) GenerateWithTools(ctx context.Context, provider ryn.Provider, req *ryn.Request) (*ryn.Stream, error) {
	if tl == nil || tl.executor == nil {
		return nil, ryn.NewError(ryn.ErrCodeInvalidRequest, "tool executor is required")
	}
	if req == nil {
		return nil, ryn.NewError(ryn.ErrCodeInvalidRequest, "request is nil")
	}

	buf := tl.opts.StreamBuffer
	if buf <= 0 {
		buf = 16
	}
	out, em := ryn.NewStream(buf)
	go func() {
		defer em.Close()
		if err := tl.run(ctx, provider, req, em); err != nil {
			em.Error(err)
		}
	}()
	return out, nil
}

func (tl *ToolLoop) run(ctx context.Context, provider ryn.Provider, req *ryn.Request, out *ryn.Emitter) error {
	cur := *req
	messages := append([]ryn.Message(nil), req.Messages...)

	var lastResp *ryn.ResponseMeta
	for round := 0; round < tl.opts.MaxRounds; round++ {
		cur.Messages = messages

		stream, err := provider.Generate(ctx, &cur)
		if err != nil {
			return err
		}

		assistantText := make([]byte, 0, 512)
		toolCalls := make([]ryn.ToolCall, 0, 2)
		for stream.Next(ctx) {
			f := stream.Frame()
			if f.Kind == ryn.KindText {
				assistantText = append(assistantText, f.Text...)
			}
			if f.Kind == ryn.KindToolCall && f.Tool != nil {
				toolCalls = append(toolCalls, *f.Tool)
			}
			if err := out.Emit(ctx, f); err != nil {
				return err
			}
		}
		if err := stream.Err(); err != nil {
			return err
		}
		lastResp = stream.Response()

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

		results := tl.executeTools(ctx, toolCalls)
		for i := range results {
			tr := results[i]
			messages = append(messages, ryn.Message{
				Role: ryn.RoleTool,
				Parts: []ryn.Part{{
					Kind: ryn.KindToolResult,
					Result: &ryn.ToolResult{
						CallID:  tr.CallID,
						Content: tr.Content,
						IsError: tr.IsError,
					},
				}},
			})
			if tl.opts.EmitToolResults {
				res := tr
				if err := out.Emit(ctx, ryn.Frame{Kind: ryn.KindToolResult, Result: &res}); err != nil {
					return err
				}
			}
		}
	}

	out.SetResponse(&ryn.ResponseMeta{FinishReason: "tool_calls"})
	return ryn.NewErrorf(ryn.ErrCodeStreamError, "tool loop exceeded max rounds (%d)", tl.opts.MaxRounds)
}

func assistantFromRound(text string, calls []ryn.ToolCall) ryn.Message {
	parts := make([]ryn.Part, 0, 1+len(calls))
	if text != "" {
		parts = append(parts, ryn.Part{Kind: ryn.KindText, Text: text})
	}
	for i := range calls {
		call := calls[i]
		parts = append(parts, ryn.Part{Kind: ryn.KindToolCall, Tool: &call})
	}
	return ryn.Message{Role: ryn.RoleAssistant, Parts: parts}
}

func (tl *ToolLoop) executeTools(ctx context.Context, calls []ryn.ToolCall) []ryn.ToolResult {
	results := make([]ryn.ToolResult, len(calls))
	if !tl.opts.Parallel || len(calls) <= 1 {
		for i := range calls {
			results[i] = tl.execOne(ctx, calls[i])
		}
		return results
	}
	var wg sync.WaitGroup
	wg.Add(len(calls))
	for i := range calls {
		i := i
		go func() {
			defer wg.Done()
			results[i] = tl.execOne(ctx, calls[i])
		}()
	}
	wg.Wait()
	return results
}

func (tl *ToolLoop) execOne(ctx context.Context, call ryn.ToolCall) ryn.ToolResult {
	execCtx := ctx
	cancel := func() {}
	if tl.opts.ToolTimeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, tl.opts.ToolTimeout)
	}
	defer cancel()

	content, err := tl.executor.Execute(execCtx, call.Name, call.Args)
	if err != nil {
		return ryn.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true}
	}
	return ryn.ToolResult{CallID: call.ID, Content: content}
}

// StreamWithToolHandling wraps a Provider and executes tool calls automatically.
type StreamWithToolHandling struct {
	provider ryn.Provider
	loop     *ToolLoop
}

// NewStreamWithToolHandling creates a provider that automatically executes tools.
func NewStreamWithToolHandling(p ryn.Provider, executor ToolExecutor, maxRounds int) *StreamWithToolHandling {
	return &StreamWithToolHandling{
		provider: p,
		loop:     NewToolLoop(executor, maxRounds),
	}
}

// NewStreamWithToolHandlingOptions creates a provider wrapper with full options.
func NewStreamWithToolHandlingOptions(p ryn.Provider, executor ToolExecutor, opts ToolStreamOptions) *StreamWithToolHandling {
	return &StreamWithToolHandling{
		provider: p,
		loop:     NewToolLoopWithOptions(executor, opts),
	}
}

// Generate implements ryn.Provider and preserves streaming while handling tools.
func (swth *StreamWithToolHandling) Generate(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
	return swth.loop.GenerateWithTools(ctx, swth.provider, req)
}
