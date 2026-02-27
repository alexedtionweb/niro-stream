package ryn_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	assertEqual(t, err, ryn.ErrClosed)
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
