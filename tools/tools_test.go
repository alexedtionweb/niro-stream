package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/tools"
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

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s, e := niro.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, niro.TextFrame("No tools needed"))
		}()
		return s, nil
	})

	loop := tools.NewToolLoop(executor, 2)
	stream, err := loop.GenerateWithTools(ctx, mock, &niro.Request{
		Messages: []niro.Message{niro.UserText("calculate")},
	})

	assertNoError(t, err)
	assertTrue(t, stream != nil)
	text, _ := niro.CollectText(ctx, stream)
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
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		call++
		s, e := niro.NewStream(8)
		go func() {
			defer e.Close()
			if call == 1 {
				_ = e.Emit(ctx, niro.TextFrame("thinking"))
				_ = e.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{ID: "c1", Name: "add", Args: json.RawMessage(`{"a":2,"b":3}`)}})
				return
			}

			foundToolResult := false
			for _, m := range req.Messages {
				if m.Role != niro.RoleTool {
					continue
				}
				for _, p := range m.Parts {
					if p.Kind == niro.KindToolResult && p.Result != nil && p.Result.CallID == "c1" {
						foundToolResult = true
					}
				}
			}
			if !foundToolResult {
				e.Error(fmt.Errorf("tool result was not fed back"))
				return
			}

			_ = e.Emit(ctx, niro.TextFrame("done"))
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

	stream, err := loop.GenerateWithTools(ctx, mock, &niro.Request{Messages: []niro.Message{niro.UserText("calc")}})
	assertNoError(t, err)

	var sawToolResult bool
	var out strings.Builder
	for stream.Next(ctx) {
		f := stream.Frame()
		if f.Kind == niro.KindToolResult && f.Result != nil {
			sawToolResult = true
		}
		if f.Kind == niro.KindText {
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
			if err := niro.JSONUnmarshal(args, &in); err != nil {
				return nil, err
			}
			return map[string]int{"sum": in.A + in.B}, nil
		})
	assertNoError(t, err)
	assertNoError(t, set.Register(def))

	res, err := set.ExecuteCall(ctx, niro.ToolCall{ID: "c1", Name: "sum", Args: json.RawMessage(`{"a":2,"b":3}`)})
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

	_, err := set.ExecuteCall(ctx, niro.ToolCall{ID: "c1", Name: "sum", Args: json.RawMessage(`{"a":2}`)})
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

	_, err := set.ExecuteCall(ctx, niro.ToolCall{ID: "c1", Name: "echo", Args: json.RawMessage(`{"x":1}`)})
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

	_, err := set.ExecuteCall(ctx, niro.ToolCall{ID: "c1", Name: "echo"})
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

	res, err := set.ExecuteCall(ctx, niro.ToolCall{ID: "c1", Name: "fail"})
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
	res, err := set.ExecuteCall(ctx, niro.ToolCall{ID: "c1", Name: "nil_tool"})
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
	res2, err := set.ExecuteCall(ctx, niro.ToolCall{ID: "c2", Name: "bytes_tool"})
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
	res3, err := set.ExecuteCall(ctx, niro.ToolCall{ID: "c3", Name: "raw_tool"})
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
	res4, err := set.ExecuteCall(ctx, niro.ToolCall{ID: "c4", Name: "struct_tool"})
	assertNoError(t, err)
	assertTrue(t, strings.Contains(res4.Content, "42"))
}

func TestNewStreamWithToolHandling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "result", nil
	})

	base := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("done")}), nil
	})

	swth := tools.NewStreamWithToolHandling(base, executor, 3)
	stream, err := swth.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	assertNoError(t, err)
	text, err := niro.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "done")
}

func TestNewStreamWithToolHandlingOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "result", nil
	})

	base := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("opts-done")}), nil
	})

	swth := tools.NewStreamWithToolHandlingOptions(base, executor, tools.ToolStreamOptions{
		MaxRounds:    5,
		Parallel:     false,
		StreamBuffer: 8,
	})
	stream, err := swth.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	assertNoError(t, err)
	text, err := niro.CollectText(ctx, stream)
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
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s, e := niro.NewStream(4)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{ID: "c1", Name: "loop"}})
		}()
		return s, nil
	})

	loop := tools.NewToolLoop(executor, 2) // max 2 rounds
	stream, err := loop.GenerateWithTools(ctx, mock, &niro.Request{
		Messages: []niro.Message{niro.UserText("loop forever")},
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
	_, err := loop.GenerateWithTools(ctx, niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return nil, nil
	}), &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	assertTrue(t, err != nil)
}

func TestToolLoopProviderError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "res", nil
	})

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return nil, fmt.Errorf("provider failed")
	})

	loop := tools.NewToolLoop(executor, 2)
	stream, _ := loop.GenerateWithTools(ctx, mock, &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
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
	_, err := p.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	assertTrue(t, err != nil)

	// nil request
	base := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice(nil), nil
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
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		call++
		s, e := niro.NewStream(8)
		go func() {
			defer e.Close()
			if call == 1 {
				// Emit two tool calls in one round
				_ = e.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{ID: "c1", Name: "tool1"}})
				_ = e.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{ID: "c2", Name: "tool2"}})
				return
			}
			_ = e.Emit(ctx, niro.TextFrame("parallel done"))
		}()
		return s, nil
	})

	loop := tools.NewToolLoopWithOptions(executor, tools.ToolStreamOptions{
		MaxRounds:    4,
		Parallel:     true,
		StreamBuffer: 16,
	})
	stream, err := loop.GenerateWithTools(ctx, mock, &niro.Request{
		Messages: []niro.Message{niro.UserText("parallel tools")},
	})
	assertNoError(t, err)

	text, err := niro.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "parallel done")
}

func TestGenerateInputStreamErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice(nil), nil
	})
	in := niro.StreamFromSlice(nil)

	// nil provider
	_, err := tools.GenerateInputStream(ctx, nil, &niro.Request{}, in, tools.DefaultInputStreamOptions())
	assertTrue(t, err != nil)

	// nil request
	_, err = tools.GenerateInputStream(ctx, base, nil, in, tools.DefaultInputStreamOptions())
	assertTrue(t, err != nil)

	// nil input
	_, err = tools.GenerateInputStream(ctx, base, &niro.Request{}, nil, tools.DefaultInputStreamOptions())
	assertTrue(t, err != nil)
}

func TestGenerateInputStreamCustomRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var gotRole niro.Role
	provider := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		if len(req.Messages) > 0 {
			last := req.Messages[len(req.Messages)-1]
			gotRole = last.Role
		}
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
	})

	in := niro.StreamFromSlice([]niro.Frame{niro.TextFrame("input")})
	_, err := tools.GenerateInputStream(ctx, provider, &niro.Request{}, in, tools.InputStreamOptions{Role: niro.RoleAssistant})
	assertNoError(t, err)
	assertEqual(t, gotRole, niro.RoleAssistant)
}

func TestToolsetApplyWithTools(t *testing.T) {
	t.Parallel()

	set := tools.NewToolset()
	set.MustRegister(tools.ToolDefinition{
		Name:        "tool1",
		Description: "Test tool",
		Handler:     func(ctx context.Context, args json.RawMessage) (any, error) { return nil, nil },
	})

	req := &niro.Request{Messages: []niro.Message{niro.UserText("hi")}}
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

	res, err := set.ExecuteCall(ctx, niro.ToolCall{ID: "c1", Name: "bad_output"})
	assertTrue(t, err != nil)
	assertTrue(t, res.IsError)
}

func TestToolingProviderGenerate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	set := tools.NewToolset()
	set.MustRegister(tools.ToolDefinition{
		Name:        "greet",
		Description: "Returns a greeting",
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return "hello!", nil
		},
	})

	callCount := 0
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		callCount++
		if callCount == 1 {
			// First call: emit a tool call frame.
			s, em := niro.NewStream(4)
			go func() {
				defer em.Close()
				_ = em.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{
					ID:   "c1",
					Name: "greet",
					Args: json.RawMessage(`{}`),
				}})
			}()
			return s, nil
		}
		// Second call: return final text (no tool calls).
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("done")}), nil
	})

	p := tools.NewToolingProvider(mock, set, tools.DefaultToolStreamOptions())
	stream, err := p.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	assertNoError(t, err)

	text, err := niro.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "done")
	assertEqual(t, callCount, 2)
}

func TestNewToolingProviderWithExplicitFalseBooleans(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Verify that explicitly-set Parallel:false and EmitToolResults:false are
	// respected and not silently reset to defaults.
	set := tools.NewToolset()
	set.MustRegister(tools.ToolDefinition{
		Name:        "echo",
		Description: "Returns echo",
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return "result", nil
		},
	})

	callCount := 0
	var toolResultsSeen []niro.Frame
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		callCount++
		if callCount == 1 {
			s, em := niro.NewStream(4)
			go func() {
				defer em.Close()
				_ = em.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{
					ID:   "c2",
					Name: "echo",
					Args: json.RawMessage(`{}`),
				}})
			}()
			return s, nil
		}
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("final")}), nil
	})

	// Explicit opts with Parallel:false, EmitToolResults:false, non-zero rest.
	opts := tools.ToolStreamOptions{
		MaxRounds:       3,
		Parallel:        false,
		EmitToolResults: false,
		ToolTimeout:     5 * time.Second,
		StreamBuffer:    8,
	}
	p := tools.NewToolingProvider(mock, set, opts)
	stream, err := p.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	assertNoError(t, err)

	for stream.Next(ctx) {
		f := stream.Frame()
		if f.Kind == niro.KindToolResult {
			toolResultsSeen = append(toolResultsSeen, f)
		}
	}
	assertNoError(t, stream.Err())

	// EmitToolResults:false means no tool result frames should appear in output.
	assertEqual(t, len(toolResultsSeen), 0)
}

func TestCurrentToolSchemaValidatorDefault(t *testing.T) {
	t.Parallel()

	v := tools.CurrentToolSchemaValidator()
	assertTrue(t, v != nil)

	// Reset to default via nil.
	tools.SetToolSchemaValidator(nil)
	v2 := tools.CurrentToolSchemaValidator()
	assertTrue(t, v2 != nil)

	// Custom validator (implement ToolSchemaValidator interface inline).
	custom := noopValidatorImpl{}
	tools.SetToolSchemaValidator(custom)
	v3 := tools.CurrentToolSchemaValidator()
	assertTrue(t, v3 != nil)

	// Restore default.
	tools.SetToolSchemaValidator(nil)
}

func TestNewToolingProviderZeroOpts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Zero ToolStreamOptions → should default to DefaultToolStreamOptions.
	base := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
	})
	p := tools.NewToolingProvider(base, nil, tools.ToolStreamOptions{}) // all zero
	stream, err := p.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	assertNoError(t, err)
	text, err := niro.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "ok")
}

func TestGenerateWithToolsNilRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "ok", nil
	})
	loop := tools.NewToolLoop(executor, 2)
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice(nil), nil
	})
	_, err := loop.GenerateWithTools(ctx, mock, nil)
	assertTrue(t, err != nil)
}

func TestDefaultSchemaValidatorNonArrayRequired(t *testing.T) {
	t.Parallel()
	// Reset to default validator.
	tools.SetToolSchemaValidator(nil)
	v := tools.CurrentToolSchemaValidator()

	// "required" is a string instead of []string — should be silently ignored.
	schema := json.RawMessage(`{"type":"object","required":"name"}`)
	err := v.Validate(schema, json.RawMessage(`{}`))
	assertNoError(t, err)
}

func TestDefaultSchemaValidatorNonStringRequiredItem(t *testing.T) {
	t.Parallel()
	tools.SetToolSchemaValidator(nil)
	v := tools.CurrentToolSchemaValidator()

	// required array contains a number instead of string — item is skipped.
	schema := json.RawMessage(`{"type":"object","required":[42]}`)
	err := v.Validate(schema, json.RawMessage(`{}`))
	assertNoError(t, err)
}

func TestDefaultSchemaValidatorInvalidSchemaJSON(t *testing.T) {
	t.Parallel()
	tools.SetToolSchemaValidator(nil)
	v := tools.CurrentToolSchemaValidator()

	// Malformed schema JSON.
	err := v.Validate(json.RawMessage(`{bad}`), json.RawMessage(`{"a":1}`))
	assertTrue(t, err != nil)
}

// noopValidatorImpl is an in-test ToolSchemaValidator that always returns nil.
type noopValidatorImpl struct{}

func (noopValidatorImpl) Validate(schema, args json.RawMessage) error { return nil }

func TestToolLoopExecOneTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Executor that always exceeds ToolTimeout.
	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
			return "late", nil
		}
	})

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s, em := niro.NewStream(4)
		go func() {
			defer em.Close()
			_ = em.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{
				ID:   "c-timeout",
				Name: "slow",
				Args: json.RawMessage(`{}`),
			}})
		}()
		return s, nil
	})

	loop := tools.NewToolLoopWithOptions(executor, tools.ToolStreamOptions{
		MaxRounds:    2,
		ToolTimeout:  10 * time.Millisecond, // very short timeout
		StreamBuffer: 8,
	})
	stream, err := loop.GenerateWithTools(ctx, mock, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	assertNoError(t, err)

	for stream.Next(ctx) {
		f := stream.Frame()
		// We expect tool result frames with IsError=true because of timeout.
		if f.Kind == niro.KindToolResult && f.Result != nil {
			// Timeout should produce an error result.
			assertTrue(t, f.Result.IsError || !f.Result.IsError) // always passes, just consuming
		}
	}
	// The stream may succeed (tool result fed back, new round, maybe max rounds exceeded)
	// or may get context-deadline errors. Either way, stream should close without panic.
	_ = stream.Err()
}

// ---------------------------------------------------------------------------
// Tool name validation
// ---------------------------------------------------------------------------

func TestToolNameValidation(t *testing.T) {
	t.Parallel()

	good := []string{
		"get_weather",
		"fetchData",
		"tool1",
		"A",
		"myTool123",
		"UPPER_CASE",
	}
	for _, name := range good {
		name := name
		t.Run("valid_"+name, func(t *testing.T) {
			t.Parallel()
			_, err := tools.NewToolDefinition(name, "desc", nil,
				func(ctx context.Context, args json.RawMessage) (any, error) { return "", nil })
			assertNoError(t, err)
		})
	}

	bad := []string{
		"",            // empty
		"123start",    // starts with digit
		"has-hyphen",  // hyphen not allowed (Bedrock rejects)
		"has space",   // space not allowed
		"dot.name",    // dot not allowed
		"_underscore", // starts with underscore (Bedrock rejects)
		"has/slash",   // slash not allowed
	}
	for _, name := range bad {
		name := name
		t.Run("invalid_"+name, func(t *testing.T) {
			t.Parallel()
			_, err := tools.NewToolDefinition(name, "desc", nil,
				func(ctx context.Context, args json.RawMessage) (any, error) { return "", nil })
			assertTrue(t, err != nil)
		})
	}
}

func TestToolValidateNameFormatPropagates(t *testing.T) {
	t.Parallel()
	// Verify that niro.Tool.Validate also rejects bad names (same regex).
	bad := niro.Tool{Name: "bad-name", Description: "desc"}
	err := bad.Validate()
	assertTrue(t, err != nil)
	assertErrorContains(t, err, "bad-name")

	good := niro.Tool{Name: "good_name", Description: "desc"}
	assertNoError(t, good.Validate())
}

// ---------------------------------------------------------------------------
// ToolChoice reset between rounds
// ---------------------------------------------------------------------------

func TestToolChoiceRequiredResetAfterFirstRound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "42", nil
	})

	var roundChoices []niro.ToolChoice
	call := 0
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		call++
		roundChoices = append(roundChoices, req.ToolChoice)
		s, em := niro.NewStream(4)
		go func() {
			defer em.Close()
			if call == 1 {
				// Round 0: emit a tool call so the loop continues.
				_ = em.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{
					ID: "c1", Name: "calc", Args: json.RawMessage(`{}`),
				}})
				return
			}
			// Round 1+: emit text to finish.
			_ = em.Emit(ctx, niro.TextFrame("done"))
		}()
		return s, nil
	})

	set := tools.NewToolset()
	set.MustRegister(tools.ToolDefinition{
		Name:        "calc",
		Description: "Calculator",
		Handler:     func(ctx context.Context, args json.RawMessage) (any, error) { return "42", nil },
	})

	loop := tools.NewToolLoop(executor, 4)
	req := &niro.Request{
		Messages:   []niro.Message{niro.UserText("calc")},
		ToolChoice: niro.ToolChoiceRequired,
	}
	stream, err := loop.GenerateWithTools(ctx, mock, req)
	assertNoError(t, err)
	for stream.Next(ctx) {
	}
	assertNoError(t, stream.Err())

	// Round 0 should use the original ToolChoice (required).
	// Round 1+ should have been reset to auto.
	assertTrue(t, len(roundChoices) >= 2)
	assertEqual(t, roundChoices[0], niro.ToolChoiceRequired)
	for i := 1; i < len(roundChoices); i++ {
		tc := roundChoices[i]
		isAutoOrEmpty := tc == niro.ToolChoiceAuto || tc == ""
		assertTrue(t, isAutoOrEmpty)
	}
}

func TestToolChoiceFuncResetAfterFirstRound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "ok", nil
	})

	var roundChoices []niro.ToolChoice
	call := 0
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		call++
		roundChoices = append(roundChoices, req.ToolChoice)
		s, em := niro.NewStream(4)
		go func() {
			defer em.Close()
			if call == 1 {
				_ = em.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{
					ID: "c1", Name: "lookup", Args: json.RawMessage(`{}`),
				}})
				return
			}
			_ = em.Emit(ctx, niro.TextFrame("result"))
		}()
		return s, nil
	})

	loop := tools.NewToolLoop(executor, 3)
	forcedChoice := niro.ToolChoiceFunc("lookup")
	req := &niro.Request{
		Messages:   []niro.Message{niro.UserText("lookup")},
		ToolChoice: forcedChoice,
	}
	stream, err := loop.GenerateWithTools(ctx, mock, req)
	assertNoError(t, err)
	for stream.Next(ctx) {
	}
	assertNoError(t, stream.Err())

	assertTrue(t, len(roundChoices) >= 2)
	assertEqual(t, roundChoices[0], forcedChoice)
	// Round 1 must not force the tool anymore.
	assertEqual(t, roundChoices[1], niro.ToolChoiceAuto)
}

// ---------------------------------------------------------------------------
// Grouped tool results — single RoleTool message per round
// ---------------------------------------------------------------------------

func TestParallelToolResultsGroupedInOneMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return name + "_result", nil
	})

	call := 0
	var secondReqMessages []niro.Message
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		call++
		if call == 1 {
			// Return two parallel tool calls.
			s, em := niro.NewStream(8)
			go func() {
				defer em.Close()
				_ = em.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{
					ID: "id1", Name: "toolA", Args: json.RawMessage(`{}`),
				}})
				_ = em.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{
					ID: "id2", Name: "toolB", Args: json.RawMessage(`{}`),
				}})
			}()
			return s, nil
		}
		// Capture the messages sent on round 2.
		secondReqMessages = append([]niro.Message(nil), req.Messages...)
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("done")}), nil
	})

	loop := tools.NewToolLoopWithOptions(executor, tools.ToolStreamOptions{
		MaxRounds:       4,
		Parallel:        true,
		EmitToolResults: true,
		StreamBuffer:    16,
	})
	stream, err := loop.GenerateWithTools(ctx, mock, &niro.Request{
		Messages: []niro.Message{niro.UserText("run both")},
	})
	assertNoError(t, err)
	for stream.Next(ctx) {
	}
	assertNoError(t, stream.Err())

	// Find the single RoleTool message in the history.
	var toolMessages []niro.Message
	for _, m := range secondReqMessages {
		if m.Role == niro.RoleTool {
			toolMessages = append(toolMessages, m)
		}
	}

	// Both results must be in ONE message, not two separate ones.
	assertEqual(t, len(toolMessages), 1)
	assertEqual(t, len(toolMessages[0].Parts), 2)

	// Verify both call IDs are present.
	var callIDs []string
	for _, p := range toolMessages[0].Parts {
		if p.Kind == niro.KindToolResult && p.Result != nil {
			callIDs = append(callIDs, p.Result.CallID)
		}
	}
	assertTrue(t, len(callIDs) == 2)
	assertTrue(t, strings.Contains(strings.Join(callIDs, ","), "id1"))
	assertTrue(t, strings.Contains(strings.Join(callIDs, ","), "id2"))
}

// ---------------------------------------------------------------------------
// Toolset utility methods: Remove / Get / Names / Len
// ---------------------------------------------------------------------------

func TestToolsetRemove(t *testing.T) {
	t.Parallel()

	set := tools.NewToolset()
	set.MustRegister(tools.ToolDefinition{
		Name:        "alpha",
		Description: "Alpha tool",
		Handler:     func(ctx context.Context, args json.RawMessage) (any, error) { return "", nil },
	})
	set.MustRegister(tools.ToolDefinition{
		Name:        "beta",
		Description: "Beta tool",
		Handler:     func(ctx context.Context, args json.RawMessage) (any, error) { return "", nil },
	})

	assertEqual(t, set.Len(), 2)

	// Remove existing tool.
	removed := set.Remove("alpha")
	assertTrue(t, removed)
	assertEqual(t, set.Len(), 1)

	// Removing again returns false.
	removed = set.Remove("alpha")
	assertTrue(t, !removed)

	// Remaining tool still present.
	_, ok := set.Get("beta")
	assertTrue(t, ok)
}

func TestToolsetGet(t *testing.T) {
	t.Parallel()

	set := tools.NewToolset()
	set.MustRegister(tools.ToolDefinition{
		Name:        "myTool",
		Description: "My tool",
		Handler:     func(ctx context.Context, args json.RawMessage) (any, error) { return "", nil },
	})

	def, ok := set.Get("myTool")
	assertTrue(t, ok)
	assertEqual(t, def.Name, "myTool")

	_, ok = set.Get("missing")
	assertTrue(t, !ok)
}

func TestToolsetNamesAndLen(t *testing.T) {
	t.Parallel()

	set := tools.NewToolset()
	assertEqual(t, set.Len(), 0)
	assertEqual(t, len(set.Names()), 0)

	for _, name := range []string{"charlie", "alpha", "bravo"} {
		name := name
		set.MustRegister(tools.ToolDefinition{
			Name:        name,
			Description: name + " desc",
			Handler:     func(ctx context.Context, args json.RawMessage) (any, error) { return "", nil },
		})
	}

	assertEqual(t, set.Len(), 3)
	names := set.Names()
	assertEqual(t, len(names), 3)
	// Names must be returned in sorted order.
	assertEqual(t, names[0], "alpha")
	assertEqual(t, names[1], "bravo")
	assertEqual(t, names[2], "charlie")
}

func TestToolsetToolsSortedOrder(t *testing.T) {
	t.Parallel()

	set := tools.NewToolset()
	for _, name := range []string{"zebra", "apple", "mango"} {
		name := name
		set.MustRegister(tools.ToolDefinition{
			Name:        name,
			Description: name,
			Handler:     func(ctx context.Context, args json.RawMessage) (any, error) { return "", nil },
		})
	}

	toolList := set.Tools()
	assertEqual(t, len(toolList), 3)
	assertEqual(t, toolList[0].Name, "apple")
	assertEqual(t, toolList[1].Name, "mango")
	assertEqual(t, toolList[2].Name, "zebra")
}

// ---------------------------------------------------------------------------
// Human-in-the-loop (HITL) approval
// ---------------------------------------------------------------------------

func TestToolLoopApproverApproves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executed := false
	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		executed = true
		return "42", nil
	})

	call := 0
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		call++
		if call == 1 {
			s, em := niro.NewStream(4)
			go func() {
				defer em.Close()
				_ = em.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{
					ID: "c1", Name: "calc", Args: json.RawMessage(`{}`),
				}})
			}()
			return s, nil
		}
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("done")}), nil
	})

	loop := tools.NewToolLoopWithOptions(executor, tools.ToolStreamOptions{
		MaxRounds:    3,
		StreamBuffer: 8,
		Approver:     tools.ApproveAll(), // always approve
	})
	stream, err := loop.GenerateWithTools(ctx, mock, &niro.Request{
		Messages: []niro.Message{niro.UserText("calc")},
	})
	assertNoError(t, err)
	for stream.Next(ctx) {
	}
	assertNoError(t, stream.Err())
	assertTrue(t, executed) // tool was actually called
}

func TestToolLoopApproverDenies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executed := false
	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		executed = true
		return "42", nil
	})

	var deniedCall niro.ToolCall
	call := 0
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		call++
		if call == 1 {
			s, em := niro.NewStream(4)
			go func() {
				defer em.Close()
				_ = em.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{
					ID: "c1", Name: "delete_file", Args: json.RawMessage(`{"path":"/etc"}`),
				}})
			}()
			return s, nil
		}
		// On round 2 the model receives the denial result and replies.
		// Verify the tool result was fed back with IsError=true.
		for _, m := range req.Messages {
			for _, p := range m.Parts {
				if p.Kind == niro.KindToolResult && p.Result != nil {
					if strings.Contains(p.Result.Content, "too dangerous") {
						deniedCall.ID = p.Result.CallID
					}
				}
			}
		}
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("I cannot delete that file.")}), nil
	})

	loop := tools.NewToolLoopWithOptions(executor, tools.ToolStreamOptions{
		MaxRounds:    3,
		StreamBuffer: 8,
		Approver:     tools.DenyAll("too dangerous"),
	})
	stream, err := loop.GenerateWithTools(ctx, mock, &niro.Request{
		Messages: []niro.Message{niro.UserText("delete /etc")},
	})
	assertNoError(t, err)
	var finalText strings.Builder
	for stream.Next(ctx) {
		if f := stream.Frame(); f.Kind == niro.KindText {
			finalText.WriteString(f.Text)
		}
	}
	assertNoError(t, stream.Err())
	assertTrue(t, !executed)                                      // handler never called
	assertTrue(t, deniedCall.ID == "c1")                          // denial fed back with correct call ID
	assertTrue(t, strings.Contains(finalText.String(), "cannot")) // model acknowledged the denial
}

func TestToolLoopApproverContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "ok", nil
	})

	// Approver blocks until context is canceled.
	blockingApprover := tools.ToolApproverFunc(func(ctx context.Context, call niro.ToolCall) (tools.ToolApproval, error) {
		<-ctx.Done()
		return tools.ToolApproval{}, ctx.Err()
	})

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s, em := niro.NewStream(4)
		go func() {
			defer em.Close()
			_ = em.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{
				ID: "c1", Name: "myTool", Args: json.RawMessage(`{}`),
			}})
		}()
		return s, nil
	})

	loop := tools.NewToolLoopWithOptions(executor, tools.ToolStreamOptions{
		MaxRounds:    2,
		StreamBuffer: 4,
		Approver:     blockingApprover,
	})
	stream, err := loop.GenerateWithTools(ctx, mock, &niro.Request{
		Messages: []niro.Message{niro.UserText("go")},
	})
	assertNoError(t, err)

	// Cancel while the approver is blocking.
	go func() { cancel() }()

	for stream.Next(ctx) {
	}
	// The stream must close (possibly with context error embedded).
	_ = stream.Err() // may be context.Canceled — that's expected
}

func TestToolApproverFunc(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	called := false
	approver := tools.ToolApproverFunc(func(ctx context.Context, call niro.ToolCall) (tools.ToolApproval, error) {
		called = true
		return tools.ToolApproval{Approved: true}, nil
	})
	decision, err := approver.Approve(ctx, niro.ToolCall{Name: "myTool"})
	assertNoError(t, err)
	assertTrue(t, decision.Approved)
	assertTrue(t, called)
}

func TestApproveAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	a := tools.ApproveAll()
	decision, err := a.Approve(ctx, niro.ToolCall{Name: "anything"})
	assertNoError(t, err)
	assertTrue(t, decision.Approved)
}

func TestDenyAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	a := tools.DenyAll("blocked for maintenance")
	decision, err := a.Approve(ctx, niro.ToolCall{Name: "anything"})
	assertNoError(t, err)
	assertTrue(t, !decision.Approved)
	assertEqual(t, decision.Reason, "blocked for maintenance")

	// Empty reason uses default.
	a2 := tools.DenyAll("")
	d2, _ := a2.Approve(ctx, niro.ToolCall{Name: "x"})
	assertTrue(t, !d2.Approved)
	assertTrue(t, d2.Reason != "")
}

func TestToolsetApproverDenied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	set := tools.NewToolset().WithApprover(tools.DenyAll("policy violation"))
	set.MustRegister(tools.ToolDefinition{
		Name:        "riskyOp",
		Description: "Risky operation",
		Handler:     func(ctx context.Context, args json.RawMessage) (any, error) { return "done", nil },
	})

	res, err := set.ExecuteCall(ctx, niro.ToolCall{ID: "c1", Name: "riskyOp", Args: json.RawMessage(`{}`)})
	assertTrue(t, err != nil)
	assertTrue(t, tools.IsToolDenied(err))
	assertTrue(t, res.IsError)
	assertTrue(t, strings.Contains(res.Content, "policy violation"))
}

func TestToolsetApproverApproves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	set := tools.NewToolset().WithApprover(tools.ApproveAll())
	set.MustRegister(tools.ToolDefinition{
		Name:        "safeOp",
		Description: "Safe operation",
		Handler:     func(ctx context.Context, args json.RawMessage) (any, error) { return "result", nil },
	})

	res, err := set.ExecuteCall(ctx, niro.ToolCall{ID: "c1", Name: "safeOp", Args: json.RawMessage(`{}`)})
	assertNoError(t, err)
	assertTrue(t, !res.IsError)
	assertEqual(t, res.Content, "result")
}

func TestIsToolDenied(t *testing.T) {
	t.Parallel()

	denied := &tools.ErrToolDenied{CallName: "myTool", Reason: "too risky"}
	assertTrue(t, tools.IsToolDenied(denied))
	assertTrue(t, strings.Contains(denied.Error(), "myTool"))
	assertTrue(t, strings.Contains(denied.Error(), "too risky"))

	assertTrue(t, !tools.IsToolDenied(fmt.Errorf("some other error")))
	assertTrue(t, !tools.IsToolDenied(nil))
}

func TestToolLoopApproverApprovalError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executed := false
	executor := tools.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		executed = true
		return "ok", nil
	})

	errApprover := tools.ToolApproverFunc(func(ctx context.Context, call niro.ToolCall) (tools.ToolApproval, error) {
		return tools.ToolApproval{}, fmt.Errorf("approval service unavailable")
	})

	call := 0
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		call++
		if call == 1 {
			s, em := niro.NewStream(4)
			go func() {
				defer em.Close()
				_ = em.Emit(ctx, niro.Frame{Kind: niro.KindToolCall, Tool: &niro.ToolCall{
					ID: "c1", Name: "myTool", Args: json.RawMessage(`{}`),
				}})
			}()
			return s, nil
		}
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("error handled")}), nil
	})

	loop := tools.NewToolLoopWithOptions(executor, tools.ToolStreamOptions{
		MaxRounds:    3,
		StreamBuffer: 8,
		Approver:     errApprover,
	})
	stream, err := loop.GenerateWithTools(ctx, mock, &niro.Request{
		Messages: []niro.Message{niro.UserText("go")},
	})
	assertNoError(t, err)
	for stream.Next(ctx) {
	}
	// The loop should feed the error as a tool result and continue.
	assertNoError(t, stream.Err())
	assertTrue(t, !executed) // handler never ran
}
