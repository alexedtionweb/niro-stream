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

func TestNewToolDefinitionAny(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"x": map[string]any{"type": "integer"},
		},
	}

	def, err := tools.NewToolDefinitionAny("myTool", "My tool description", schema,
		func(ctx context.Context, args json.RawMessage) (any, error) {
			return "result", nil
		})
	assertNoError(t, err)
	assertEqual(t, def.Name, "myTool")

	// Execute via Toolset
	set := tools.NewToolset()
	assertNoError(t, set.Register(def))
	res, err := set.Execute(ctx, "myTool", json.RawMessage(`{"x":5}`))
	assertNoError(t, err)
	assertEqual(t, res, "result")
}

func TestNewToolDefinitionAnyMarshalError(t *testing.T) {
	t.Parallel()

	// channels can't be marshaled
	_, err := tools.NewToolDefinitionAny("t", "desc", make(chan int), func(ctx context.Context, args json.RawMessage) (any, error) {
		return nil, nil
	})
	assertTrue(t, err != nil)
}

func TestSetToolSchemaValidator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Set custom validator that always passes
	custom := &testSchemaValidator{validateFn: func(schema, args json.RawMessage) error {
		return nil
	}}
	tools.SetToolSchemaValidator(custom)
	defer tools.SetToolSchemaValidator(nil) // reset to default

	v := tools.CurrentToolSchemaValidator()
	assertTrue(t, v != nil)
	assertNoError(t, v.Validate(nil, nil))

	// Set nil resets to default
	tools.SetToolSchemaValidator(nil)
	v2 := tools.CurrentToolSchemaValidator()
	assertTrue(t, v2 != nil)

	// Default validator rejects bad args
	err := v2.Validate(nil, json.RawMessage(`{bad}`))
	assertTrue(t, err != nil)

	_ = ctx
}

func TestToolsetWithValidator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	set := tools.NewToolset()
	alwaysFail := &testSchemaValidator{validateFn: func(schema, args json.RawMessage) error {
		return fmt.Errorf("validation always fails")
	}}
	set.WithValidator(alwaysFail)
	set.MustRegister(tools.ToolDefinition{
		Name:        "echo",
		Description: "Echo",
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return "ok", nil
		},
	})

	_, err := set.ExecuteCall(ctx, ryn.ToolCall{ID: "c1", Name: "echo"})
	assertTrue(t, err != nil)
	assertErrorContains(t, err, "always fails")
}

func TestToolsetApplyNilReq(t *testing.T) {
	t.Parallel()
	set := tools.NewToolset()
	result := set.Apply(nil)
	assertTrue(t, result == nil)
}

func TestToolsetExecuteToolNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	set := tools.NewToolset()
	_, err := set.Execute(ctx, "nonexistent", nil)
	assertTrue(t, err != nil)
	assertErrorContains(t, err, "not found")
}

func TestToolsetExecuteCallHandlerError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	set := tools.NewToolset()
	set.MustRegister(tools.ToolDefinition{
		Name:        "fail",
		Description: "Always fails",
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return nil, fmt.Errorf("handler error")
		},
	})

	res, err := set.ExecuteCall(ctx, ryn.ToolCall{ID: "c1", Name: "fail"})
	assertTrue(t, err != nil)
	assertTrue(t, res.IsError)
	assertErrorContains(t, err, "handler error")
}

func TestNormalizeToolOutputTypes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// nil output
	set := tools.NewToolset()
	set.MustRegister(tools.ToolDefinition{
		Name:        "nil_tool",
		Description: "Returns nil",
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return nil, nil
		},
	})
	res, err := set.ExecuteCall(ctx, ryn.ToolCall{ID: "c1", Name: "nil_tool"})
	assertNoError(t, err)
	assertEqual(t, res.Content, "")

	// []byte output
	set.MustRegister(tools.ToolDefinition{
		Name:        "bytes_tool",
		Description: "Returns bytes",
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return []byte("hello bytes"), nil
		},
	})
	res2, err := set.ExecuteCall(ctx, ryn.ToolCall{ID: "c2", Name: "bytes_tool"})
	assertNoError(t, err)
	assertEqual(t, res2.Content, "hello bytes")

	// json.RawMessage output
	set.MustRegister(tools.ToolDefinition{
		Name:        "raw_tool",
		Description: "Returns raw JSON",
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return json.RawMessage(`{"k":"v"}`), nil
		},
	})
	res3, err := set.ExecuteCall(ctx, ryn.ToolCall{ID: "c3", Name: "raw_tool"})
	assertNoError(t, err)
	assertEqual(t, res3.Content, `{"k":"v"}`)

	// struct output (JSON marshaled)
	set.MustRegister(tools.ToolDefinition{
		Name:        "struct_tool",
		Description: "Returns struct",
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return map[string]int{"val": 42}, nil
		},
	})
	res4, err := set.ExecuteCall(ctx, ryn.ToolCall{ID: "c4", Name: "struct_tool"})
	assertNoError(t, err)
	assertTrue(t, strings.Contains(res4.Content, "42"))
}

func TestNewStreamWithToolHandling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "result", nil
	})

	base := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("done")}), nil
	})

	swth := tools.NewStreamWithToolHandling(base, executor, 3)
	stream, err := swth.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}})
	assertNoError(t, err)
	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "done")
}

func TestNewStreamWithToolHandlingOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "result", nil
	})

	base := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("opts-done")}), nil
	})

	swth := tools.NewStreamWithToolHandlingOptions(base, executor, tools.ToolStreamOptions{
		MaxRounds:    5,
		Parallel:     false,
		StreamBuffer: 8,
	})
	stream, err := swth.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}})
	assertNoError(t, err)
	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "opts-done")
}

func TestToolLoopMaxRoundsExceeded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "res", nil
	})

	// Provider always returns a tool call, never terminates
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(4)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.Frame{Kind: ryn.KindToolCall, Tool: &ryn.ToolCall{ID: "c1", Name: "loop"}})
		}()
		return s, nil
	})

	loop := tools.NewToolLoop(executor, 2) // max 2 rounds
	stream, err := loop.GenerateWithTools(ctx, mock, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("loop forever")},
	})
	assertNoError(t, err) // stream creation is fine

	// consume - should get error
	for stream.Next(ctx) {
	}
	assertTrue(t, stream.Err() != nil)
	assertErrorContains(t, stream.Err(), "max rounds")
}

func TestToolLoopNilExecutor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	loop := tools.NewToolLoop(nil, 2)
	_, err := loop.GenerateWithTools(ctx, ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, nil
	}), &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}})
	assertTrue(t, err != nil)
}

func TestToolLoopProviderError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "res", nil
	})

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, fmt.Errorf("provider failed")
	})

	loop := tools.NewToolLoop(executor, 2)
	stream, _ := loop.GenerateWithTools(ctx, mock, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	for stream.Next(ctx) {
	}
	assertTrue(t, stream.Err() != nil)
}

func TestToolingProviderNilCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// nil provider
	p := tools.NewToolingProvider(nil, nil, tools.DefaultToolStreamOptions())
	_, err := p.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}})
	assertTrue(t, err != nil)

	// nil request
	base := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice(nil), nil
	})
	p2 := tools.NewToolingProvider(base, nil, tools.DefaultToolStreamOptions())
	_, err2 := p2.Generate(ctx, nil)
	assertTrue(t, err2 != nil)
}

func TestToolsetMustRegisterPanic(t *testing.T) {
	t.Parallel()

	set := tools.NewToolset()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	set.MustRegister(tools.ToolDefinition{}) // empty name → panic
}

func TestToolsetWithNilValidator(t *testing.T) {
	t.Parallel()
	set := tools.NewToolset()
	result := set.WithValidator(nil)
	assertTrue(t, result == set) // returns self unchanged
}

func TestToolsetWithNilHook(t *testing.T) {
	t.Parallel()
	set := tools.NewToolset()
	result := set.WithHook(nil)
	assertTrue(t, result == set)
}

func TestToolDefinitionValidate(t *testing.T) {
	t.Parallel()

	// missing name
	d := tools.ToolDefinition{Description: "desc", Handler: func(ctx context.Context, args json.RawMessage) (any, error) { return nil, nil }}
	assertTrue(t, d.Validate() != nil)

	// missing description
	d2 := tools.ToolDefinition{Name: "n", Handler: func(ctx context.Context, args json.RawMessage) (any, error) { return nil, nil }}
	assertTrue(t, d2.Validate() != nil)

	// nil handler
	d3 := tools.ToolDefinition{Name: "n", Description: "desc"}
	assertTrue(t, d3.Validate() != nil)

	// invalid schema
	d4 := tools.ToolDefinition{
		Name: "n", Description: "desc",
		Schema:  json.RawMessage(`{invalid}`),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) { return nil, nil },
	}
	assertTrue(t, d4.Validate() != nil)
}

func TestToolDefinitionToTool(t *testing.T) {
	t.Parallel()

	def, _ := tools.NewToolDefinition("mytool", "does stuff", json.RawMessage(`{"type":"object"}`),
		func(ctx context.Context, args json.RawMessage) (any, error) { return nil, nil })
	tool := def.ToTool()
	assertEqual(t, tool.Name, "mytool")
	assertEqual(t, tool.Description, "does stuff")
}

func TestToolsetTools(t *testing.T) {
	t.Parallel()

	set := tools.NewToolset()
	set.MustRegister(tools.ToolDefinition{
		Name:        "a",
		Description: "tool A",
		Handler:     func(ctx context.Context, args json.RawMessage) (any, error) { return nil, nil },
	})
	set.MustRegister(tools.ToolDefinition{
		Name:        "b",
		Description: "tool B",
		Handler:     func(ctx context.Context, args json.RawMessage) (any, error) { return nil, nil },
	})

	toolList := set.Tools()
	assertEqual(t, len(toolList), 2)
}

func TestToolLoopParallelMultipleCalls(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "res:" + name, nil
	})

	call := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		call++
		s, e := ryn.NewStream(8)
		go func() {
			defer e.Close()
			if call == 1 {
				// Emit two tool calls in one round
				_ = e.Emit(ctx, ryn.Frame{Kind: ryn.KindToolCall, Tool: &ryn.ToolCall{ID: "c1", Name: "tool1"}})
				_ = e.Emit(ctx, ryn.Frame{Kind: ryn.KindToolCall, Tool: &ryn.ToolCall{ID: "c2", Name: "tool2"}})
				return
			}
			_ = e.Emit(ctx, ryn.TextFrame("parallel done"))
		}()
		return s, nil
	})

	loop := tools.NewToolLoopWithOptions(executor, tools.ToolStreamOptions{
		MaxRounds:    4,
		Parallel:     true,
		StreamBuffer: 16,
	})
	stream, err := loop.GenerateWithTools(ctx, mock, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("parallel tools")},
	})
	assertNoError(t, err)

	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "parallel done")
}

func TestGenerateInputStreamErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice(nil), nil
	})
	in := ryn.StreamFromSlice(nil)

	// nil provider
	_, err := tools.GenerateInputStream(ctx, nil, &ryn.Request{}, in, tools.DefaultInputStreamOptions())
	assertTrue(t, err != nil)

	// nil request
	_, err = tools.GenerateInputStream(ctx, base, nil, in, tools.DefaultInputStreamOptions())
	assertTrue(t, err != nil)

	// nil input
	_, err = tools.GenerateInputStream(ctx, base, &ryn.Request{}, nil, tools.DefaultInputStreamOptions())
	assertTrue(t, err != nil)
}

func TestGenerateInputStreamCustomRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var gotRole ryn.Role
	provider := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		if len(req.Messages) > 0 {
			last := req.Messages[len(req.Messages)-1]
			gotRole = last.Role
		}
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	in := ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("input")})
	_, err := tools.GenerateInputStream(ctx, provider, &ryn.Request{}, in, tools.InputStreamOptions{Role: ryn.RoleAssistant})
	assertNoError(t, err)
	assertEqual(t, gotRole, ryn.RoleAssistant)
}

func TestToolsetApplyWithTools(t *testing.T) {
	t.Parallel()

	set := tools.NewToolset()
	set.MustRegister(tools.ToolDefinition{
		Name:        "tool1",
		Description: "Test tool",
		Handler:     func(ctx context.Context, args json.RawMessage) (any, error) { return nil, nil },
	})

	req := &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}}
	applied := set.Apply(req)

	// original is unchanged
	assertEqual(t, len(req.Tools), 0)
	// applied has the tool
	assertEqual(t, len(applied.Tools), 1)
	assertEqual(t, applied.Tools[0].Name, "tool1")
}

func TestToolsetNormalizeOutputUnmarshalError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A channel can't be marshaled - triggers error in normalizeToolOutput
	set := tools.NewToolset()
	set.MustRegister(tools.ToolDefinition{
		Name:        "bad_output",
		Description: "Returns an unmarshalable value",
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return make(chan int), nil // can't be JSON marshaled
		},
	})

	res, err := set.ExecuteCall(ctx, ryn.ToolCall{ID: "c1", Name: "bad_output"})
	assertTrue(t, err != nil)
	assertTrue(t, res.IsError)
}
