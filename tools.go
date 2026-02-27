package ryn

import (
	"context"
	"encoding/json"
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
	executor  ToolExecutor
	maxRounds int // Maximum rounds of tool calling to prevent infinite loops
}

// NewToolLoop creates a ToolLoop with the given executor.
func NewToolLoop(executor ToolExecutor, maxRounds int) *ToolLoop {
	if maxRounds <= 0 {
		maxRounds = 10
	}
	return &ToolLoop{
		executor:  executor,
		maxRounds: maxRounds,
	}
}

// GenerateWithTools sends a request and runs the tool loop until completion.
// Returns the final stream after all tool rounds have completed.
func (tl *ToolLoop) GenerateWithTools(ctx context.Context, provider Provider, req *Request) (*Stream, error) {
	// Initial generation
	stream, err := provider.Generate(ctx, req)
	if err != nil {
		return nil, err
	}

	// Collect all frames from initial response
	var text string
	var toolCalls []ToolCall
	usage := Usage{}

	for stream.Next(ctx) {
		frame := stream.Frame()
		switch frame.Kind {
		case KindText:
			text += frame.Text
		case KindToolCall:
			if frame.Tool != nil {
				toolCalls = append(toolCalls, *frame.Tool)
			}
		case KindUsage:
			if frame.Usage != nil {
				usage.Add(frame.Usage)
			}
		}
	}

	if err := stream.Err(); err != nil {
		return nil, err
	}

	// If no tool calls, we're done
	if len(toolCalls) == 0 {
		// Return a stream with the collected content
		return streamFromCollected(text, toolCalls, &usage), nil
	}

	// Run tool calling loop
	messages := append(req.Messages, AssistantText(text))
	if len(toolCalls) > 0 {
		// Assistant message with tool calls (if supported by provider)
		// For now, add as text representation
		for _, tc := range toolCalls {
			messages = append(messages, AssistantText("Calling tool: "+tc.Name))
		}
	}

	// Execute each tool call
	for round := 0; round < tl.maxRounds; round++ {
		select {
		case <-ctx.Done():
			return nil, NewError(ErrCodeContextCancelled, "context cancelled during tool execution")
		default:
		}

		// Execute tool calls and collect results
		var toolResults []Message
		for _, tc := range toolCalls {
			result, err := tl.executor.Execute(ctx, tc.Name, tc.Args)
			if err != nil {
				toolResults = append(toolResults, ToolErrorMessage(tc.ID, err.Error()))
			} else {
				toolResults = append(toolResults, ToolMessage(tc.ID, result))
			}
		}

		messages = append(messages, toolResults...)

		// Request next generation with tool results
		nextReq := *req
		nextReq.Messages = messages
		nextStream, err := provider.Generate(ctx, &nextReq)
		if err != nil {
			return nil, err
		}

		// Collect next response
		text = ""
		toolCalls = nil
		nextUsage := Usage{}

		for nextStream.Next(ctx) {
			frame := nextStream.Frame()
			switch frame.Kind {
			case KindText:
				text += frame.Text
			case KindToolCall:
				if frame.Tool != nil {
					toolCalls = append(toolCalls, *frame.Tool)
				}
			case KindUsage:
				if frame.Usage != nil {
					nextUsage.Add(frame.Usage)
				}
			}
		}

		if err := nextStream.Err(); err != nil {
			return nil, err
		}

		usage.Add(&nextUsage)

		// Add this turn's response to messages
		messages = append(messages, AssistantText(text))

		// If no tool calls in this round, we're done
		if len(toolCalls) == 0 {
			return streamFromCollected(text, toolCalls, &usage), nil
		}
	}

	// Max rounds exceeded
	return nil, NewErrorf(ErrCodeStreamError, "tool loop exceeded max rounds (%d)", tl.maxRounds)
}

// streamFromCollected creates a stream from collected data.
func streamFromCollected(text string, toolCalls []ToolCall, usage *Usage) *Stream {
	s, e := NewStream(0)
	go func() {
		defer e.Close()

		if text != "" {
			for _, ch := range text {
				_ = e.Emit(context.Background(), TextFrame(string(ch)))
			}
		}

		for _, tc := range toolCalls {
			tc := tc
			_ = e.Emit(context.Background(), Frame{Kind: KindToolCall, Tool: &tc})
		}

		if usage != nil && (usage.InputTokens > 0 || usage.OutputTokens > 0) {
			_ = e.Emit(context.Background(), Frame{Kind: KindUsage, Usage: usage})
		}

		e.SetResponse(&ResponseMeta{
			FinishReason: "tool_calls",
		})
	}()
	return s
}

// StreamWithToolHandling wraps a Provider to automatically handle tool calls.
// It intercepts tool call frames and executes them, emitting results back into the stream.
type StreamWithToolHandling struct {
	provider  Provider
	executor  ToolExecutor
	maxRounds int
}

// NewStreamWithToolHandling creates a provider that automatically executes tools.
func NewStreamWithToolHandling(p Provider, executor ToolExecutor, maxRounds int) *StreamWithToolHandling {
	if maxRounds <= 0 {
		maxRounds = 10
	}
	return &StreamWithToolHandling{
		provider:  p,
		executor:  executor,
		maxRounds: maxRounds,
	}
}

// Generate implements Provider, handling tool calls automatically.
func (swth *StreamWithToolHandling) Generate(ctx context.Context, req *Request) (*Stream, error) {
	tl := NewToolLoop(swth.executor, swth.maxRounds)
	return tl.GenerateWithTools(ctx, swth.provider, req)
}
