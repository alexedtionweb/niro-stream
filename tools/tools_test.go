package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"ryn.dev/ryn"
	"ryn.dev/ryn/tools"
)

func TestToolLoopBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		if name == "add" {
			return "5", nil
		}
		return "", fmt.Errorf("unknown tool")
	})

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.TextFrame("No tools needed"))
		}()
		return s, nil
	})

	loop := tools.NewToolLoop(executor, 2)
	stream, err := loop.GenerateWithTools(ctx, mock, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("calculate")},
	})

	assertNoError(t, err)
	assertTrue(t, stream != nil)
	text, _ := ryn.CollectText(ctx, stream)
	assertTrue(t, len(text) > 0)
}

func TestToolLoopFeedsResultsAndStreams(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		if name != "add" {
			return "", fmt.Errorf("unknown tool")
		}
		return `{"result":5}`, nil
	})

	call := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		call++
		s, e := ryn.NewStream(8)
		go func() {
			defer e.Close()
			if call == 1 {
				_ = e.Emit(ctx, ryn.TextFrame("thinking"))
				_ = e.Emit(ctx, ryn.Frame{Kind: ryn.KindToolCall, Tool: &ryn.ToolCall{ID: "c1", Name: "add", Args: json.RawMessage(`{"a":2,"b":3}`)}})
				return
			}

			foundToolResult := false
			for _, m := range req.Messages {
				if m.Role != ryn.RoleTool {
					continue
				}
				for _, p := range m.Parts {
					if p.Kind == ryn.KindToolResult && p.Result != nil && p.Result.CallID == "c1" {
						foundToolResult = true
					}
				}
			}
			if !foundToolResult {
				e.Error(fmt.Errorf("tool result was not fed back"))
				return
			}

			_ = e.Emit(ctx, ryn.TextFrame("done"))
		}()
		return s, nil
	})

	loop := tools.NewToolLoopWithOptions(executor, tools.ToolStreamOptions{
		MaxRounds:       4,
		Parallel:        true,
		EmitToolResults: true,
		ToolTimeout:     time.Second,
		StreamBuffer:    16,
	})

	stream, err := loop.GenerateWithTools(ctx, mock, &ryn.Request{Messages: []ryn.Message{ryn.UserText("calc")}})
	assertNoError(t, err)

	var sawToolResult bool
	var out strings.Builder
	for stream.Next(ctx) {
		f := stream.Frame()
		if f.Kind == ryn.KindToolResult && f.Result != nil {
			sawToolResult = true
		}
		if f.Kind == ryn.KindText {
			out.WriteString(f.Text)
		}
	}
	assertNoError(t, stream.Err())
	assertTrue(t, sawToolResult)
	assertEqual(t, out.String(), "thinkingdone")
}

func TestToolsetDefineValidateExecute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	set := tools.NewToolset()
	def, err := tools.NewToolDefinition("sum", "Sum two numbers", json.RawMessage(`{"type":"object","required":["a","b"]}`),
		func(ctx context.Context, args json.RawMessage) (any, error) {
			var in struct {
				A int `json:"a"`
				B int `json:"b"`
			}
			if err := ryn.JSONUnmarshal(args, &in); err != nil {
				return nil, err
			}
			return map[string]int{"sum": in.A + in.B}, nil
		})
	assertNoError(t, err)
	assertNoError(t, set.Register(def))

	res, err := set.ExecuteCall(ctx, ryn.ToolCall{ID: "c1", Name: "sum", Args: json.RawMessage(`{"a":2,"b":3}`)})
	assertNoError(t, err)
	assertEqual(t, res.IsError, false)
	assertTrue(t, strings.Contains(res.Content, "5"))
}

func TestToolsetValidationFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	set := tools.NewToolset()
	set.MustRegister(tools.ToolDefinition{
		Name:        "sum",
		Description: "Sum",
		Schema:      json.RawMessage(`{"type":"object","required":["a","b"]}`),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return "ok", nil
		},
	})

	_, err := set.ExecuteCall(ctx, ryn.ToolCall{ID: "c1", Name: "sum", Args: json.RawMessage(`{"a":2}`)})
	assertNotNil(t, err)
	assertTrue(t, strings.Contains(err.Error(), "missing required field"))
}

func TestToolsetHooks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := &toolHookMock{}
	set := tools.NewToolset().WithHook(h)
	set.MustRegister(tools.ToolDefinition{
		Name:        "echo",
		Description: "Echo",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return "ok", nil
		},
	})

	_, err := set.ExecuteCall(ctx, ryn.ToolCall{ID: "c1", Name: "echo", Args: json.RawMessage(`{"x":1}`)})
	assertNoError(t, err)
	assertEqual(t, h.validateCalls, 1)
	assertEqual(t, h.startCalls, 1)
	assertEqual(t, h.endCalls, 1)
}

func TestToolingProviderAppliesTools(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	set := tools.NewToolset()
	set.MustRegister(tools.ToolDefinition{
		Name:        "echo",
		Description: "Echo",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return "done", nil
		},
	})

	base := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		if len(req.Tools) == 0 {
			return nil, fmt.Errorf("expected tools to be applied")
		}
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	p := tools.NewToolingProvider(base, set, tools.DefaultToolStreamOptions())
	stream, err := p.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}})
	assertNoError(t, err)
	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "ok")
}
