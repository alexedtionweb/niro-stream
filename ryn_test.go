package ryn_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ryn.dev/ryn"
)

// --- Frame Tests ---

func TestFrameConstructors(t *testing.T) {
	t.Parallel()

	t.Run("TextFrame", func(t *testing.T) {
		f := ryn.TextFrame("hello")
		assertEqual(t, f.Kind, ryn.KindText)
		assertEqual(t, f.Text, "hello")
	})

	t.Run("AudioFrame", func(t *testing.T) {
		f := ryn.AudioFrame([]byte{1, 2}, "audio/pcm")
		assertEqual(t, f.Kind, ryn.KindAudio)
		assertEqual(t, len(f.Data), 2)
		assertEqual(t, f.Mime, "audio/pcm")
	})

	t.Run("ImageFrame", func(t *testing.T) {
		f := ryn.ImageFrame([]byte{0xFF}, "image/png")
		assertEqual(t, f.Kind, ryn.KindImage)
	})

	t.Run("ToolCallFrame", func(t *testing.T) {
		tc := &ryn.ToolCall{ID: "c1", Name: "fn", Args: json.RawMessage(`{}`)}
		f := ryn.ToolCallFrame(tc)
		assertEqual(t, f.Kind, ryn.KindToolCall)
		assertEqual(t, f.Tool.Name, "fn")
	})

	t.Run("ToolResultFrame", func(t *testing.T) {
		tr := &ryn.ToolResult{CallID: "c1", Content: "ok"}
		f := ryn.ToolResultFrame(tr)
		assertEqual(t, f.Kind, ryn.KindToolResult)
		assertEqual(t, f.Result.Content, "ok")
	})

	t.Run("UsageFrame", func(t *testing.T) {
		u := &ryn.Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}
		f := ryn.UsageFrame(u)
		assertEqual(t, f.Kind, ryn.KindUsage)
		assertEqual(t, f.Usage.TotalTokens, 30)
	})

	t.Run("ControlFrame", func(t *testing.T) {
		f := ryn.ControlFrame(ryn.SignalFlush)
		assertEqual(t, f.Kind, ryn.KindControl)
		assertEqual(t, f.Signal, ryn.SignalFlush)
	})
}

func TestKindString(t *testing.T) {
	t.Parallel()
	assertEqual(t, ryn.KindText.String(), "text")
	assertEqual(t, ryn.KindToolCall.String(), "tool_call")
	assertEqual(t, ryn.KindUsage.String(), "usage")
	assertEqual(t, ryn.Kind(0).String(), "unknown")
}

func TestSignalString(t *testing.T) {
	t.Parallel()
	assertEqual(t, ryn.SignalFlush.String(), "flush")
	assertEqual(t, ryn.SignalEOT.String(), "eot")
	assertEqual(t, ryn.SignalAbort.String(), "abort")
	assertEqual(t, ryn.SignalNone.String(), "none")
}

func TestUsageAdd(t *testing.T) {
	t.Parallel()
	u := ryn.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	u.Add(&ryn.Usage{
		InputTokens:  20,
		OutputTokens: 10,
		TotalTokens:  30,
		Detail:       map[string]int{"cached": 5},
	})
	assertEqual(t, u.InputTokens, 30)
	assertEqual(t, u.OutputTokens, 15)
	assertEqual(t, u.TotalTokens, 45)
	assertEqual(t, u.Detail["cached"], 5)

	// Add with detail again
	u.Add(&ryn.Usage{Detail: map[string]int{"cached": 3, "reasoning": 10}})
	assertEqual(t, u.Detail["cached"], 8)
	assertEqual(t, u.Detail["reasoning"], 10)

	// Add nil is safe
	u.Add(nil)
	assertEqual(t, u.InputTokens, 30)
}

func TestToolChoice(t *testing.T) {
	t.Parallel()
	assertEqual(t, string(ryn.ToolChoiceAuto), "auto")
	assertEqual(t, string(ryn.ToolChoiceNone), "none")
	assertEqual(t, string(ryn.ToolChoiceRequired), "required")
	assertEqual(t, string(ryn.ToolChoiceFunc("weather")), "func:weather")
}

// --- Message Tests ---

func TestMessageConstructors(t *testing.T) {
	t.Parallel()

	t.Run("UserText", func(t *testing.T) {
		m := ryn.UserText("hi")
		assertEqual(t, m.Role, ryn.RoleUser)
		assertEqual(t, len(m.Parts), 1)
		assertEqual(t, m.Parts[0].Text, "hi")
	})

	t.Run("SystemText", func(t *testing.T) {
		m := ryn.SystemText("be brief")
		assertEqual(t, m.Role, ryn.RoleSystem)
		assertEqual(t, m.Parts[0].Text, "be brief")
	})

	t.Run("AssistantText", func(t *testing.T) {
		m := ryn.AssistantText("ok")
		assertEqual(t, m.Role, ryn.RoleAssistant)
		assertEqual(t, m.Parts[0].Text, "ok")
	})

	t.Run("ToolMessage", func(t *testing.T) {
		m := ryn.ToolMessage("call1", `{"temp":72}`)
		assertEqual(t, m.Role, ryn.RoleTool)
		assertEqual(t, m.Parts[0].Result.CallID, "call1")
		assertEqual(t, m.Parts[0].Result.IsError, false)
	})

	t.Run("ToolErrorMessage", func(t *testing.T) {
		m := ryn.ToolErrorMessage("call2", "failed")
		assertEqual(t, m.Parts[0].Result.IsError, true)
	})

	t.Run("Multi", func(t *testing.T) {
		m := ryn.Multi(ryn.RoleUser,
			ryn.TextPart("check this image"),
			ryn.ImageURLPart("https://example.com/img.png", "image/png"),
		)
		assertEqual(t, len(m.Parts), 2)
		assertEqual(t, m.Parts[1].URL, "https://example.com/img.png")
	})
}

// --- Stream Tests ---

func TestStreamBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, e := ryn.NewStream(4)
	go func() {
		defer e.Close()
		e.Emit(ctx, ryn.TextFrame("Hello"))
		e.Emit(ctx, ryn.TextFrame(" World"))
	}()

	text, err := ryn.CollectText(ctx, s)
	assertNoError(t, err)
	assertEqual(t, text, "Hello World")
}

func TestStreamUsageAutoAccumulation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, e := ryn.NewStream(8)
	go func() {
		defer e.Close()
		e.Emit(ctx, ryn.TextFrame("a"))
		e.Emit(ctx, ryn.UsageFrame(&ryn.Usage{InputTokens: 10, OutputTokens: 1, TotalTokens: 11}))
		e.Emit(ctx, ryn.TextFrame("b"))
		e.Emit(ctx, ryn.UsageFrame(&ryn.Usage{InputTokens: 0, OutputTokens: 1, TotalTokens: 1}))
	}()

	var frames []ryn.Frame
	for s.Next(ctx) {
		frames = append(frames, s.Frame())
	}

	// Usage frames should NOT appear in output
	assertEqual(t, len(frames), 2)
	assertEqual(t, frames[0].Text, "a")
	assertEqual(t, frames[1].Text, "b")

	// But usage should be accumulated
	usage := s.Usage()
	assertEqual(t, usage.InputTokens, 10)
	assertEqual(t, usage.OutputTokens, 2)
	assertEqual(t, usage.TotalTokens, 12)
}

func TestStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, e := ryn.NewStream(4)
	go func() {
		e.Emit(ctx, ryn.TextFrame("a"))
		e.Error(errTest)
	}()

	var texts []string
	for s.Next(ctx) {
		texts = append(texts, s.Frame().Text)
	}

	assertEqual(t, len(texts), 1)
	assertErrorContains(t, s.Err(), "test error")
}

func TestStreamContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	s, e := ryn.NewStream(0)
	go func() {
		defer e.Close()
		for i := 0; i < 100; i++ {
			if err := e.Emit(ctx, ryn.TextFrame("x")); err != nil {
				return
			}
		}
	}()

	s.Next(ctx)
	cancel()

	// Drain should eventually stop
	for s.Next(ctx) {
	}
	assertErrorContains(t, s.Err(), "context canceled")
}

func TestStreamFromSlice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("a"),
		ryn.TextFrame("b"),
	})

	frames, err := ryn.Collect(ctx, s)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
	assertEqual(t, frames[1].Text, "b")
}

func TestStreamResponse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, e := ryn.NewStream(4)
	go func() {
		e.SetResponse(&ryn.ResponseMeta{
			Model:        "gpt-4o",
			FinishReason: "stop",
			ID:           "resp_123",
		})
		e.Close()
	}()

	_, err := ryn.Collect(ctx, s)
	assertNoError(t, err)

	resp := s.Response()
	assertNotNil(t, resp)
	assertEqual(t, resp.Model, "gpt-4o")
	assertEqual(t, resp.FinishReason, "stop")
}

func TestEmitAfterClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, e := ryn.NewStream(0)
	e.Close()

	err := e.Emit(ctx, ryn.TextFrame("late"))
	assertErrorContains(t, err, "stream closed")
}

func TestCollectText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, e := ryn.NewStream(8)
	go func() {
		defer e.Close()
		e.Emit(ctx, ryn.TextFrame("Hello"))
		e.Emit(ctx, ryn.ControlFrame(ryn.SignalFlush))
		e.Emit(ctx, ryn.TextFrame(" World"))
		e.Emit(ctx, ryn.AudioFrame(nil, ""))
	}()

	text, err := ryn.CollectText(ctx, s)
	assertNoError(t, err)
	assertEqual(t, text, "Hello World")
}

// --- Provider Tests ---

func TestProviderFunc(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(2)
		go func() {
			defer e.Close()
			e.Emit(ctx, ryn.TextFrame("mock: "+req.Messages[0].Parts[0].Text))
		}()
		return s, nil
	})

	stream, err := mock.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("ping")},
	})
	assertNoError(t, err)

	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "mock: ping")
}

func TestRequestEffectiveMessages(t *testing.T) {
	t.Parallel()

	t.Run("NoSystemPrompt", func(t *testing.T) {
		req := &ryn.Request{
			Messages: []ryn.Message{ryn.UserText("hi")},
		}
		msgs := req.EffectiveMessages()
		assertEqual(t, len(msgs), 1)
	})

	t.Run("WithSystemPrompt", func(t *testing.T) {
		req := &ryn.Request{
			SystemPrompt: "be helpful",
			Messages:     []ryn.Message{ryn.UserText("hi")},
		}
		msgs := req.EffectiveMessages()
		assertEqual(t, len(msgs), 2)
		assertEqual(t, msgs[0].Role, ryn.RoleSystem)
		assertEqual(t, msgs[0].Parts[0].Text, "be helpful")
	})
}

// --- Processor Tests ---

func TestPassThrough(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("a"),
		ryn.TextFrame("b"),
	})

	out, emitter := ryn.NewStream(4)
	go func() {
		defer emitter.Close()
		ryn.PassThrough().Process(ctx, in, emitter)
	}()

	frames, err := ryn.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
}

func TestFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("keep"),
		ryn.AudioFrame(nil, ""),
		ryn.TextFrame("also keep"),
	})

	out, emitter := ryn.NewStream(4)
	go func() {
		defer emitter.Close()
		ryn.Filter(func(f ryn.Frame) bool { return f.Kind == ryn.KindText }).Process(ctx, in, emitter)
	}()

	frames, err := ryn.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
}

func TestMap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("hello")})

	out, emitter := ryn.NewStream(4)
	go func() {
		defer emitter.Close()
		ryn.Map(func(f ryn.Frame) ryn.Frame {
			if f.Kind == ryn.KindText {
				f.Text = "[" + f.Text + "]"
			}
			return f
		}).Process(ctx, in, emitter)
	}()

	text, err := ryn.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "[hello]")
}

func TestTap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("a"),
		ryn.TextFrame("b"),
	})

	var count int
	out, emitter := ryn.NewStream(4)
	go func() {
		defer emitter.Close()
		ryn.Tap(func(f ryn.Frame) { count++ }).Process(ctx, in, emitter)
	}()

	frames, err := ryn.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
	assertEqual(t, count, 2)
}

func TestTextOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("keep"),
		ryn.ControlFrame(ryn.SignalFlush),
		ryn.ImageFrame(nil, ""),
	})

	out, emitter := ryn.NewStream(4)
	go func() {
		defer emitter.Close()
		ryn.TextOnly().Process(ctx, in, emitter)
	}()

	frames, err := ryn.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 1)
	assertEqual(t, frames[0].Text, "keep")
}

func TestAccumulate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("H"),
		ryn.TextFrame("e"),
		ryn.TextFrame("l"),
		ryn.ControlFrame(ryn.SignalFlush),
		ryn.TextFrame("l"),
		ryn.TextFrame("o"),
	})

	out, emitter := ryn.NewStream(4)
	go func() {
		defer emitter.Close()
		ryn.Accumulate().Process(ctx, in, emitter)
	}()

	frames, err := ryn.Collect(ctx, out)
	assertNoError(t, err)
	// 1 control frame (passed through) + 1 accumulated text frame
	assertEqual(t, len(frames), 2)

	// Find the text frame
	var text string
	for _, f := range frames {
		if f.Kind == ryn.KindText {
			text = f.Text
		}
	}
	assertEqual(t, text, "Hello")
}

// --- Pipeline Tests ---

func TestPipelineEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("x")})
	out := ryn.Pipe().Run(ctx, in)

	frames, err := ryn.Collect(ctx, out)
	assertNoError(t, err)
	assertEqual(t, len(frames), 1)
}

func TestPipelineSingleStage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("a"),
		ryn.TextFrame("b"),
	})

	p := ryn.Pipe(
		ryn.Map(func(f ryn.Frame) ryn.Frame {
			f.Text = f.Text + "!"
			return f
		}),
	)

	out := p.Run(ctx, in)
	text, err := ryn.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "a!b!")
}

func TestPipelineMultiStage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("hello"),
		ryn.AudioFrame(nil, ""),
		ryn.TextFrame(" world"),
	})

	p := ryn.Pipe(
		ryn.TextOnly(),
		ryn.Map(func(f ryn.Frame) ryn.Frame {
			f.Text = "[" + f.Text + "]"
			return f
		}),
	).WithBuffer(8)

	out := p.Run(ctx, in)
	text, err := ryn.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "[hello][ world]")
}

func TestPipelineWithAccumulate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("H"),
		ryn.TextFrame("i"),
	})

	p := ryn.Pipe(ryn.Accumulate())
	out := p.Run(ctx, in)

	text, err := ryn.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "Hi")
}

// --- Hook Tests ---

func TestNoOpHookSatisfiesInterface(t *testing.T) {
	t.Parallel()
	var h ryn.Hook = ryn.NoOpHook{}
	ctx := h.OnGenerateStart(context.Background(), ryn.GenerateStartInfo{})
	assertNotNil(t, ctx)
	assertEqual(t, h.OnFrame(ctx, ryn.TextFrame("x")), nil)
}

func TestHooksComposition(t *testing.T) {
	t.Parallel()

	var count1, count2 atomic.Int32

	h1 := &countingHook{onFrame: func() { count1.Add(1) }}
	h2 := &countingHook{onFrame: func() { count2.Add(1) }}

	combined := ryn.Hooks(h1, nil, h2)
	ctx := context.Background()

	combined.OnGenerateStart(ctx, ryn.GenerateStartInfo{})
	combined.OnFrame(ctx, ryn.TextFrame("a"))
	combined.OnFrame(ctx, ryn.TextFrame("b"))

	assertEqual(t, int(count1.Load()), 2)
	assertEqual(t, int(count2.Load()), 2)
}

func TestHooksSingleNonNil(t *testing.T) {
	t.Parallel()
	h := &countingHook{}
	combined := ryn.Hooks(nil, h, nil)
	// Should return the single non-nil hook directly
	ctx := combined.OnGenerateStart(context.Background(), ryn.GenerateStartInfo{})
	assertNotNil(t, ctx)
}

func TestHooksAllNil(t *testing.T) {
	t.Parallel()
	combined := ryn.Hooks(nil, nil)
	assertNil(t, combined)
}

// --- Orchestration Tests ---

func TestFan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stream := ryn.Fan(ctx,
		func(ctx context.Context) (*ryn.Stream, error) {
			return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("A")}), nil
		},
		func(ctx context.Context) (*ryn.Stream, error) {
			return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("B")}), nil
		},
	)

	frames, err := ryn.Collect(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)

	// Both tokens should be present (order may vary)
	texts := map[string]bool{}
	for _, f := range frames {
		texts[f.Text] = true
	}
	assertTrue(t, texts["A"])
	assertTrue(t, texts["B"])
}

func TestRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	text, usage, err := ryn.Race(ctx,
		func(ctx context.Context) (*ryn.Stream, error) {
			time.Sleep(50 * time.Millisecond) // slow
			s, e := ryn.NewStream(4)
			go func() {
				defer e.Close()
				e.Emit(ctx, ryn.TextFrame("slow"))
				e.Emit(ctx, ryn.UsageFrame(&ryn.Usage{TotalTokens: 10}))
			}()
			return s, nil
		},
		func(ctx context.Context) (*ryn.Stream, error) {
			s, e := ryn.NewStream(4) // fast
			go func() {
				defer e.Close()
				e.Emit(ctx, ryn.TextFrame("fast"))
				e.Emit(ctx, ryn.UsageFrame(&ryn.Usage{TotalTokens: 5}))
			}()
			return s, nil
		},
	)

	assertNoError(t, err)
	assertEqual(t, text, "fast")
	assertEqual(t, usage.TotalTokens, 5)
}

func TestSequence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stream, err := ryn.Sequence(ctx,
		func(ctx context.Context, input string) (*ryn.Stream, error) {
			s, e := ryn.NewStream(2)
			go func() {
				defer e.Close()
				e.Emit(ctx, ryn.TextFrame("step1"))
			}()
			return s, nil
		},
		func(ctx context.Context, input string) (*ryn.Stream, error) {
			s, e := ryn.NewStream(2)
			go func() {
				defer e.Close()
				e.Emit(ctx, ryn.TextFrame(input+"+step2"))
			}()
			return s, nil
		},
	)

	assertNoError(t, err)
	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "step1+step2")
}

func TestSequenceEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stream, err := ryn.Sequence(ctx)
	assertNoError(t, err)

	frames, err := ryn.Collect(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, len(frames), 0)
}

// --- Runtime Tests ---

func TestRuntimeBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(4)
		go func() {
			defer e.Close()
			e.Emit(ctx, ryn.TextFrame("hello"))
			e.SetResponse(&ryn.ResponseMeta{Model: "test", FinishReason: "stop"})
		}()
		return s, nil
	})

	rt := ryn.NewRuntime(mock)
	stream, err := rt.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	assertNoError(t, err)

	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "hello")
}

func TestRuntimeWithPipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(4)
		go func() {
			defer e.Close()
			e.Emit(ctx, ryn.TextFrame("H"))
			e.Emit(ctx, ryn.TextFrame("i"))
		}()
		return s, nil
	})

	rt := ryn.NewRuntime(mock).WithPipeline(ryn.Pipe(ryn.Accumulate()))
	stream, err := rt.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("test")},
	})
	assertNoError(t, err)

	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "Hi")
}

func TestRuntimeWithHook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var (
		startCalled atomic.Bool
		endCalled   atomic.Bool
		frameCnt    atomic.Int32
	)

	hook := &testHook{
		onStart: func(ctx context.Context, info ryn.GenerateStartInfo) context.Context {
			startCalled.Store(true)
			assertEqual(t, info.Model, "test-model")
			return ctx
		},
		onEnd: func(ctx context.Context, info ryn.GenerateEndInfo) {
			endCalled.Store(true)
			assertEqual(t, info.Model, "test-model")
		},
		onFrame: func(ctx context.Context, f ryn.Frame) error {
			frameCnt.Add(1)
			return nil
		},
	}

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(4)
		go func() {
			defer e.Close()
			e.Emit(ctx, ryn.TextFrame("a"))
			e.Emit(ctx, ryn.TextFrame("b"))
			e.SetResponse(&ryn.ResponseMeta{Model: "test-model"})
		}()
		return s, nil
	})

	rt := ryn.NewRuntime(mock).WithHook(hook)
	stream, err := rt.Generate(ctx, &ryn.Request{
		Model:    "test-model",
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	assertNoError(t, err)

	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "ab")

	// Give the goroutine a moment to fire OnGenerateEnd
	time.Sleep(20 * time.Millisecond)

	assertTrue(t, startCalled.Load())
	assertTrue(t, endCalled.Load())
	assertEqual(t, int(frameCnt.Load()), 2)
}

// --- Forward Tests ---

func TestForward(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	src := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("x"),
		ryn.TextFrame("y"),
	})

	dst, dstEmitter := ryn.NewStream(4)
	go func() {
		defer dstEmitter.Close()
		ryn.Forward(ctx, src, dstEmitter)
	}()

	frames, err := ryn.Collect(ctx, dst)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)
}

// --- BytePool Tests ---

func TestBytePoolGetPut(t *testing.T) {
	t.Parallel()
	pool := ryn.NewBytePool()

	// Small buffer
	buf := pool.Get(100)
	assertEqual(t, len(buf), 100)
	assertTrue(t, cap(buf) >= 100)
	pool.Put(buf)

	// Medium buffer
	buf = pool.Get(32 * 1024)
	assertEqual(t, len(buf), 32*1024)
	pool.Put(buf)

	// Large buffer
	buf = pool.Get(512 * 1024)
	assertEqual(t, len(buf), 512*1024)
	pool.Put(buf)

	// Huge buffer (not pooled)
	buf = pool.Get(2 * 1024 * 1024)
	assertEqual(t, len(buf), 2*1024*1024)
	pool.Put(buf)
}

func TestBytePoolConcurrent(t *testing.T) {
	t.Parallel()
	pool := ryn.NewBytePool()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				buf := pool.Get(960) // typical audio chunk
				buf[0] = 42
				pool.Put(buf)
			}
		}()
	}
	wg.Wait()
}

func TestPooledFrameConstructors(t *testing.T) {
	t.Parallel()
	pool := ryn.NewBytePool()

	data := []byte{1, 2, 3, 4, 5}

	af := ryn.AudioFramePooled(pool, data, "audio/pcm")
	assertEqual(t, af.Kind, ryn.KindAudio)
	assertEqual(t, len(af.Data), 5)
	assertEqual(t, af.Data[0], byte(1))
	assertEqual(t, af.Mime, "audio/pcm")
	pool.Put(af.Data)

	imgF := ryn.ImageFramePooled(pool, data, "image/png")
	assertEqual(t, imgF.Kind, ryn.KindImage)
	pool.Put(imgF.Data)

	vf := ryn.VideoFramePooled(pool, data, "video/mp4")
	assertEqual(t, vf.Kind, ryn.KindVideo)
	pool.Put(vf.Data)
}

func TestUsagePool(t *testing.T) {
	t.Parallel()
	u := ryn.GetUsage()
	assertEqual(t, u.InputTokens, 0)
	assertEqual(t, u.OutputTokens, 0)

	u.InputTokens = 100
	u.OutputTokens = 50
	u.Detail = map[string]int{"cached": 10}
	ryn.PutUsage(u)

	// After put, getting a new one should be zeroed
	u2 := ryn.GetUsage()
	assertEqual(t, u2.InputTokens, 0)
	assertTrue(t, u2.Detail == nil)
	ryn.PutUsage(u2)
}

func TestResponseMetaPool(t *testing.T) {
	t.Parallel()
	m := ryn.GetResponseMeta()
	assertEqual(t, m.Model, "")
	assertEqual(t, m.ID, "")

	m.Model = "gpt-4o"
	m.ProviderMeta = map[string]any{"x": 1}
	ryn.PutResponseMeta(m)

	m2 := ryn.GetResponseMeta()
	assertEqual(t, m2.Model, "")
	assertTrue(t, m2.ProviderMeta == nil)
	ryn.PutResponseMeta(m2)
}

func TestUsageReset(t *testing.T) {
	t.Parallel()
	u := ryn.Usage{
		InputTokens:  100,
		OutputTokens: 50,
		TotalTokens:  150,
		Detail:       map[string]int{"cached": 10},
	}
	u.Reset()
	assertEqual(t, u.InputTokens, 0)
	assertEqual(t, u.OutputTokens, 0)
	assertEqual(t, u.TotalTokens, 0)
	assertEqual(t, len(u.Detail), 0) // map cleared, not nil
}

// --- Cache Tests ---

func TestCacheHitMiss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	callCount := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		callCount++
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("hello")}), nil
	})

	cache := ryn.NewCache(ryn.CacheOptions{MaxEntries: 100, TTL: time.Minute})
	provider := cache.Wrap(mock)

	req := &ryn.Request{Model: "test", Messages: []ryn.Message{ryn.UserText("hi")}}

	// First call: miss
	s, err := provider.Generate(ctx, req)
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, s)
	assertEqual(t, text, "hello")
	assertEqual(t, callCount, 1)

	// Second call: hit
	s2, err := provider.Generate(ctx, req)
	assertNoError(t, err)
	text2, _ := ryn.CollectText(ctx, s2)
	assertEqual(t, text2, "hello")
	assertEqual(t, callCount, 1) // not called again

	hits, misses := cache.Stats()
	assertEqual(t, hits, int64(1))
	assertEqual(t, misses, int64(1))
}

func TestCacheDifferentRequests(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	callCount := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		callCount++
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame(req.Messages[0].Parts[0].Text)}), nil
	})

	cache := ryn.NewCache(ryn.CacheOptions{MaxEntries: 100})
	provider := cache.Wrap(mock)

	s1, _ := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("a")}})
	ryn.CollectText(ctx, s1)

	s2, _ := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("b")}})
	ryn.CollectText(ctx, s2)

	assertEqual(t, callCount, 2) // different requests, both miss
}

func TestCacheTTLExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	callCount := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		callCount++
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	cache := ryn.NewCache(ryn.CacheOptions{MaxEntries: 100, TTL: 50 * time.Millisecond})
	provider := cache.Wrap(mock)

	req := &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}}

	s1, _ := provider.Generate(ctx, req)
	ryn.CollectText(ctx, s1)
	assertEqual(t, callCount, 1)

	time.Sleep(100 * time.Millisecond)

	s2, _ := provider.Generate(ctx, req)
	ryn.CollectText(ctx, s2)
	assertEqual(t, callCount, 2) // expired, called again
}

func TestCacheLRUEviction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("x")}), nil
	})

	cache := ryn.NewCache(ryn.CacheOptions{MaxEntries: 64, TTL: time.Hour})
	provider := cache.Wrap(mock)

	// Fill cache beyond capacity
	for i := 0; i < 200; i++ {
		s, _ := provider.Generate(ctx, &ryn.Request{
			Model:    fmt.Sprintf("model-%d", i),
			Messages: []ryn.Message{ryn.UserText("hi")},
		})
		ryn.CollectText(ctx, s)
	}

	assertTrue(t, cache.Len() <= 64)
}

func TestCacheClear(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("x")}), nil
	})

	cache := ryn.NewCache(ryn.CacheOptions{MaxEntries: 100})
	provider := cache.Wrap(mock)

	for i := 0; i < 10; i++ {
		s, _ := provider.Generate(ctx, &ryn.Request{
			Model:    fmt.Sprintf("m%d", i),
			Messages: []ryn.Message{ryn.UserText("x")},
		})
		ryn.CollectText(ctx, s)
	}

	assertTrue(t, cache.Len() > 0)
	cache.Clear()
	assertEqual(t, cache.Len(), 0)
}

func TestCacheConcurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	cache := ryn.NewCache(ryn.CacheOptions{MaxEntries: 1000})
	provider := cache.Wrap(mock)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				req := &ryn.Request{
					Model:    fmt.Sprintf("m%d", j%5),
					Messages: []ryn.Message{ryn.UserText("hi")},
				}
				s, err := provider.Generate(ctx, req)
				if err != nil {
					t.Errorf("generate error: %v", err)
					return
				}
				ryn.CollectText(ctx, s)
			}
		}()
	}
	wg.Wait()
}

// --- Registry Tests ---

func TestRegistryBasic(t *testing.T) {
	t.Parallel()

	reg := ryn.NewRegistry()
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("hello")}), nil
	})

	reg.Register("test", mock)
	assertTrue(t, reg.Has("test"))
	assertEqual(t, reg.Len(), 1)

	p, err := reg.Get("test")
	assertNoError(t, err)
	assertNotNil(t, p)
}

func TestRegistryNotFound(t *testing.T) {
	t.Parallel()

	reg := ryn.NewRegistry()
	_, err := reg.Get("missing")
	assertErrorContains(t, err, "not registered")
}

func TestRegistryMustGetPanics(t *testing.T) {
	t.Parallel()

	reg := ryn.NewRegistry()
	defer func() {
		r := recover()
		assertNotNil(t, r)
	}()
	reg.MustGet("missing")
}

func TestRegistryRemove(t *testing.T) {
	t.Parallel()

	reg := ryn.NewRegistry()
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, nil
	})

	reg.Register("x", mock)
	assertTrue(t, reg.Has("x"))
	reg.Remove("x")
	assertEqual(t, reg.Has("x"), false)
}

func TestRegistryAllAndNames(t *testing.T) {
	t.Parallel()

	reg := ryn.NewRegistry()
	for _, name := range []string{"a", "b", "c"} {
		n := name
		reg.Register(n, ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
			return nil, nil
		}))
	}

	all := reg.All()
	assertEqual(t, len(all), 3)
	names := reg.Names()
	assertEqual(t, len(names), 3)
}

func TestRegistryGenerate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := ryn.NewRegistry()
	reg.Register("mock", ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("from registry")}), nil
	}))

	s, err := reg.Generate(ctx, "mock", &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}})
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, s)
	assertEqual(t, text, "from registry")
}

func TestRegistryGenerateNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := ryn.NewRegistry()
	_, err := reg.Generate(ctx, "nope", &ryn.Request{})
	assertErrorContains(t, err, "not registered")
}

func TestRegistryConcurrent(t *testing.T) {
	t.Parallel()

	reg := ryn.NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("p%d", i%10)
			reg.Register(name, ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
				return nil, nil
			}))
			reg.Has(name)
			reg.Get(name)
			reg.Names()
		}()
	}
	wg.Wait()
}

// --- Transport Tests ---

func TestTransportDefaults(t *testing.T) {
	t.Parallel()

	tr := ryn.Transport(nil)
	assertNotNil(t, tr)
	assertTrue(t, tr.MaxIdleConns > 0)
	assertTrue(t, tr.MaxIdleConnsPerHost > 0)
	assertTrue(t, tr.IdleConnTimeout > 0)
	assertTrue(t, !tr.DisableKeepAlives)
}

func TestTransportCustomOptions(t *testing.T) {
	t.Parallel()

	tr := ryn.Transport(&ryn.TransportOptions{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 25,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     30 * time.Second,
		DisableKeepAlives:   true,
	})
	assertEqual(t, tr.MaxIdleConns, 100)
	assertEqual(t, tr.MaxIdleConnsPerHost, 25)
	assertEqual(t, tr.MaxConnsPerHost, 50)
	assertEqual(t, tr.IdleConnTimeout, 30*time.Second)
	assertTrue(t, tr.DisableKeepAlives)
}

func TestHTTPClient(t *testing.T) {
	t.Parallel()
	client := ryn.HTTPClient(nil)
	assertNotNil(t, client)
	assertNotNil(t, client.Transport)
}

func TestDefaultTransportAndClient(t *testing.T) {
	t.Parallel()
	assertNotNil(t, ryn.DefaultTransport)
	assertNotNil(t, ryn.DefaultHTTPClient)
}

// --- JSON Library Tests ---

func TestSetJSON(t *testing.T) {
	t.Parallel()

	called := atomic.Int32{}
	custom := &ryn.JSONLibrary{
		Marshal: func(v any) ([]byte, error) {
			called.Add(1)
			return []byte(`{"a":1}`), nil
		},
		Unmarshal: func(data []byte, v any) error {
			called.Add(1)
			return json.Unmarshal(data, v)
		},
		Valid: func(data []byte) bool {
			called.Add(1)
			return true
		},
	}

	ryn.SetJSON(custom)
	t.Cleanup(func() { ryn.SetJSON(nil) })

	var out struct {
		A int `json:"a"`
	}

	b, err := ryn.JSONMarshal(out)
	assertNoError(t, err)
	assertTrue(t, len(b) > 0)
	assertNoError(t, ryn.JSONUnmarshal(b, &out))
	assertTrue(t, ryn.JSONValid(b))
	assertTrue(t, called.Load() >= 3)
}

// --- Structured Output Tests ---

func TestGenerateStructured(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type result struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"]}`)

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		if req.ResponseFormat != "json_schema" {
			t.Errorf("expected ResponseFormat=json_schema, got %q", req.ResponseFormat)
		}
		if string(req.ResponseSchema) != string(schema) {
			t.Errorf("schema mismatch")
		}
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.TextFrame(`{"name":"alice","age":30}`))
			_ = e.Emit(ctx, ryn.UsageFrame(&ryn.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}))
		}()
		return s, nil
	})

	res, _, usage, err := ryn.GenerateStructured[result](ctx, mock, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("hi")},
	}, schema)
	assertNoError(t, err)
	assertEqual(t, res.Name, "alice")
	assertEqual(t, res.Age, 30)
	assertEqual(t, usage.TotalTokens, 8)
}

func TestStreamStructuredPartialAndFinal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type result struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"]}`)

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.TextFrame(`{"name":"al`))
			_ = e.Emit(ctx, ryn.TextFrame(`ice","age":30}`))
		}()
		return s, nil
	})

	ss, err := ryn.StreamStructured[result](ctx, mock, &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}}, schema)
	assertNoError(t, err)

	partialSeen := false
	finalSeen := false
	for ss.Next(ctx) {
		ev := ss.Event()
		if ev.Partial != nil {
			partialSeen = true
		}
		if ev.Final != nil {
			finalSeen = true
			assertEqual(t, ev.Final.Name, "alice")
			assertEqual(t, ev.Final.Age, 30)
		}
	}
	assertTrue(t, partialSeen)
	assertTrue(t, finalSeen)
	assertNoError(t, ss.Err())
}

func TestGenerateStructuredNoText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	schema := json.RawMessage(`{"type":"object"}`)
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.UsageFrame(&ryn.Usage{InputTokens: 1}))
		}()
		return s, nil
	})

	_, _, _, err := ryn.GenerateStructured[map[string]any](ctx, mock, &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}}, schema)
	assertErrorContains(t, err, "no structured output")
}

// --- Helpers ---

type countingHook struct {
	ryn.NoOpHook
	onFrame func()
}

func (h *countingHook) OnFrame(_ context.Context, _ ryn.Frame) error {
	if h.onFrame != nil {
		h.onFrame()
	}
	return nil
}

type testHook struct {
	ryn.NoOpHook
	onStart func(context.Context, ryn.GenerateStartInfo) context.Context
	onEnd   func(context.Context, ryn.GenerateEndInfo)
	onFrame func(context.Context, ryn.Frame) error
}

func (h *testHook) OnGenerateStart(ctx context.Context, info ryn.GenerateStartInfo) context.Context {
	if h.onStart != nil {
		return h.onStart(ctx, info)
	}
	return ctx
}

func (h *testHook) OnGenerateEnd(ctx context.Context, info ryn.GenerateEndInfo) {
	if h.onEnd != nil {
		h.onEnd(ctx, info)
	}
}

func (h *testHook) OnFrame(ctx context.Context, f ryn.Frame) error {
	if h.onFrame != nil {
		return h.onFrame(ctx, f)
	}
	return nil
}

var errTest = fmt.Errorf("test error")

func assertEqual[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func assertErrorContains(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Errorf("expected error containing %q, got nil", substr)
		return
	}
	if !contains(err.Error(), substr) {
		t.Errorf("error %q does not contain %q", err.Error(), substr)
	}
}

// --- Error Handling Tests ---

func TestErrorCreation(t *testing.T) {
	t.Parallel()

	err := ryn.NewError(ryn.ErrCodeInvalidRequest, "test error")
	assertNotNil(t, err)
	assertEqual(t, err.Code, ryn.ErrCodeInvalidRequest)
	assertEqual(t, err.Message, "test error")
	assertTrue(t, err.Error() != "")
}

func TestErrorWrapping(t *testing.T) {
	t.Parallel()

	inner := fmt.Errorf("inner error")
	err := ryn.WrapError(ryn.ErrCodeProviderError, "provider failed", inner)
	assertTrue(t, err != nil)

	// Check error message contains both
	msg := err.Error()
	assertTrue(t, strings.Contains(msg, "provider failed"))
	assertTrue(t, strings.Contains(msg, "inner error"))
}

func TestErrorCheckers(t *testing.T) {
	t.Parallel()

	rateLimitErr := ryn.NewError(ryn.ErrCodeRateLimited, "too many requests")
	assertTrue(t, ryn.IsRetryable(rateLimitErr))
	assertTrue(t, ryn.IsRateLimited(rateLimitErr))
	assertEqual(t, ryn.IsTimeout(rateLimitErr), false)

	authErr := ryn.NewError(ryn.ErrCodeAuthenticationFailed, "invalid key")
	assertTrue(t, ryn.IsAuthError(authErr))
	assertEqual(t, ryn.IsRetryable(authErr), false)

	timeoutErr := ryn.NewError(ryn.ErrCodeTimeout, "took too long")
	assertTrue(t, ryn.IsTimeout(timeoutErr))
	assertTrue(t, ryn.IsRetryable(timeoutErr))
}

func TestErrorWithContext(t *testing.T) {
	t.Parallel()

	err := ryn.NewError(ryn.ErrCodeProviderError, "failed")
	err.WithProvider("openai").WithRequestID("req_123").WithStatusCode(500)
	msg := err.Error()
	assertTrue(t, strings.Contains(msg, "openai"))
	assertTrue(t, strings.Contains(msg, "req_123"))
	assertEqual(t, err.StatusCode, 500)
}

// --- Validation Tests ---

func TestRequestValidation(t *testing.T) {
	t.Parallel()

	t.Run("ValidRequest", func(t *testing.T) {
		req := &ryn.Request{
			Messages: []ryn.Message{ryn.UserText("hello")},
		}
		err := req.Validate()
		assertTrue(t, err == nil)
	})

	t.Run("NoMessages", func(t *testing.T) {
		req := &ryn.Request{}
		err := req.Validate()
		assertTrue(t, err != nil)
		assertTrue(t, strings.Contains(err.Message, "no messages"))
	})

	t.Run("InvalidResponseFormat", func(t *testing.T) {
		req := &ryn.Request{
			Messages:       []ryn.Message{ryn.UserText("hi")},
			ResponseFormat: "invalid_format",
		}
		err := req.Validate()
		assertTrue(t, err != nil)
	})

	t.Run("JsonSchemaMissingSchema", func(t *testing.T) {
		req := &ryn.Request{
			Messages:       []ryn.Message{ryn.UserText("hi")},
			ResponseFormat: "json_schema",
			ResponseSchema: nil,
		}
		err := req.Validate()
		assertTrue(t, err != nil)
		assertTrue(t, strings.Contains(err.Message, "ResponseSchema"))
	})

	t.Run("InvalidJSON Schema", func(t *testing.T) {
		req := &ryn.Request{
			Messages:       []ryn.Message{ryn.UserText("hi")},
			ResponseFormat: "json_schema",
			ResponseSchema: json.RawMessage(`{invalid}`),
		}
		err := req.Validate()
		assertTrue(t, err != nil)
		assertEqual(t, err.Code, ryn.ErrCodeInvalidSchema)
	})

	t.Run("InvalidOptions", func(t *testing.T) {
		req := &ryn.Request{
			Messages: []ryn.Message{ryn.UserText("hi")},
			Options: ryn.Options{
				Temperature: ryn.Temp(3.0), // out of range
			},
		}
		err := req.Validate()
		assertTrue(t, err != nil)
		assertTrue(t, strings.Contains(err.Message, "Temperature"))
	})
}

func TestMessageValidation(t *testing.T) {
	t.Parallel()

	t.Run("ValidMessage", func(t *testing.T) {
		msg := ryn.UserText("hello")
		err := msg.Validate()
		assertNil(t, err)
	})

	t.Run("EmptyMessage", func(t *testing.T) {
		msg := ryn.Message{Role: ryn.RoleUser, Parts: []ryn.Part{}}
		err := msg.Validate()
		assertNotNil(t, err)
	})
}

func TestToolValidation(t *testing.T) {
	t.Parallel()

	t.Run("ValidTool", func(t *testing.T) {
		tool := ryn.Tool{
			Name:        "weather",
			Description: "Get weather",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}
		err := tool.Validate()
		assertNil(t, err)
	})

	t.Run("MissingName", func(t *testing.T) {
		tool := ryn.Tool{Description: "Get weather"}
		err := tool.Validate()
		assertNotNil(t, err)
		assertTrue(t, strings.Contains(err.Error(), "Name"))
	})
}

// --- Retry Tests ---

func TestRetryProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	attempts := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		attempts++
		if attempts < 3 {
			return nil, ryn.NewError(ryn.ErrCodeRateLimited, "too fast")
		}
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	config := ryn.RetryConfig{
		MaxAttempts: 5,
		Backoff:     ryn.ConstantBackoff{Duration: 5 * time.Millisecond},
		ShouldRetry: ryn.IsRetryable,
	}

	provider := ryn.NewRetryProvider(mock, config)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})

	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, stream)
	assertEqual(t, text, "ok")
	assertEqual(t, attempts, 3)
}

func TestExponentialBackoff(t *testing.T) {
	t.Parallel()

	backoff := ryn.ExponentialBackoff{
		InitialDelay: 10 * time.Millisecond,
		Multiplier:   2.0,
		MaxDelay:     100 * time.Millisecond,
		Jitter:       false,
	}

	d0 := backoff.Delay(0)
	d1 := backoff.Delay(1)
	d2 := backoff.Delay(2)

	assertTrue(t, d0 >= 10*time.Millisecond)
	assertTrue(t, d1 > d0)
	assertTrue(t, d2 > d1)
	assertTrue(t, d2 <= 100*time.Millisecond)
}

// --- Cost Tracking Tests ---

func TestCostCalculation(t *testing.T) {
	t.Parallel()

	pricing := &ryn.ModelPricing{
		InputCostPer1M:  5.00,
		OutputCostPer1M: 15.00,
	}

	usage := ryn.Usage{
		InputTokens:  1000000, // 1M
		OutputTokens: 1000000, // 1M
		TotalTokens:  2000000,
	}

	cost := pricing.CalculateCost(usage)
	assertTrue(t, cost.TotalCost > 0)
	assertEqual(t, cost.InputCost, 5.0)
	assertEqual(t, cost.OutputCost, 15.0)
	assertEqual(t, cost.Currency, "USD")
}

func TestPricingRegistry(t *testing.T) {
	t.Parallel()

	registry := ryn.NewPricingRegistry()
	registry.Set("openai", "gpt-4o", &ryn.ModelPricing{
		InputCostPer1M:  5.00,
		OutputCostPer1M: 15.00,
	})
	registry.SetDefault("openai", &ryn.ModelPricing{
		InputCostPer1M:  1.00,
		OutputCostPer1M: 3.00,
	})

	// Exact match
	p := registry.Get("openai", "gpt-4o")
	assertTrue(t, p != nil)
	assertEqual(t, p.InputCostPer1M, 5.00)

	// Fallback to default
	p2 := registry.Get("openai", "unknown")
	assertTrue(t, p2 != nil)
	assertEqual(t, p2.InputCostPer1M, 1.00)

	// Not found
	p3 := registry.Get("unknown", "unknown")
	assertTrue(t, p3 == nil)
}

// --- Tracing Tests ---

func TestGenerateRequestID(t *testing.T) {
	t.Parallel()

	id1 := ryn.GenerateRequestID()
	id2 := ryn.GenerateRequestID()

	assertTrue(t, len(id1.String()) > 0)
	assertTrue(t, id1.String() != id2.String()) // Should be unique
	assertTrue(t, strings.HasPrefix(id1.String(), "req_"))
}

func TestTraceContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	trace := ryn.TraceContext{
		RequestID: ryn.GenerateRequestID(),
		UserID:    "user123",
		SessionID: "session456",
	}

	ctx = ryn.WithTraceContext(ctx, trace)
	retrieved := ryn.GetTraceContext(ctx)

	assertEqual(t, retrieved.RequestID, trace.RequestID)
	assertEqual(t, retrieved.UserID, "user123")
	assertEqual(t, retrieved.SessionID, "session456")
}

func TestTracingProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		// Should have trace context injected
		trace := ryn.GetTraceContext(ctx)
		assertTrue(t, trace.RequestID != "")
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	provider := ryn.NewTracingProvider(mock)
	stream, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("test")}})
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, stream)
	assertEqual(t, text, "ok")
}

// --- Tool Execution Tests ---

func TestToolLoopBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	executor := ryn.ToolExecutorFunc(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		if name == "add" {
			return "5", nil
		}
		return "", fmt.Errorf("unknown tool")
	})

	// Create a provider that returns no tool calls (so loop completes immediately)
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, ryn.TextFrame("No tools needed"))
		}()
		return s, nil
	})

	loop := ryn.NewToolLoop(executor, 2)
	stream, err := loop.GenerateWithTools(ctx, mock, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("calculate")},
	})

	assertNoError(t, err)
	assertTrue(t, stream != nil)
	text, _ := ryn.CollectText(ctx, stream)
	assertTrue(t, len(text) > 0)
}

func assertNotNil(t *testing.T, v any) {
	t.Helper()
	if v == nil {
		t.Error("expected non-nil")
	}
}

func assertNil(t *testing.T, v any) {
	t.Helper()
	if v != nil {
		t.Errorf("expected nil, got %v", v)
	}
}

func assertTrue(t *testing.T, v bool) {
	t.Helper()
	if !v {
		t.Error("expected true")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
