package ryn_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/hook"
	"github.com/alexedtionweb/niro-stream/middleware"
	"github.com/alexedtionweb/niro-stream/orchestrate"
	"github.com/alexedtionweb/niro-stream/pipe"
	"github.com/alexedtionweb/niro-stream/registry"
	"github.com/alexedtionweb/niro-stream/runtime"
	"github.com/alexedtionweb/niro-stream/structured"
)

// ─── Frame Construction ─────────────────────────────────────

func BenchmarkTextFrame(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = ryn.TextFrame("token")
	}
}

func BenchmarkAudioFrame(b *testing.B) {
	data := make([]byte, 960) // 20ms 48kHz mono PCM
	b.ReportAllocs()
	for b.Loop() {
		_ = ryn.AudioFrame(data, "audio/pcm")
	}
}

func BenchmarkToolCallFrame(b *testing.B) {
	tc := &ryn.ToolCall{ID: "call_1", Name: "get_weather", Args: json.RawMessage(`{"city":"NYC"}`)}
	b.ReportAllocs()
	for b.Loop() {
		_ = ryn.ToolCallFrame(tc)
	}
}

func BenchmarkUsageFrame(b *testing.B) {
	u := &ryn.Usage{InputTokens: 100, OutputTokens: 200, TotalTokens: 300}
	b.ReportAllocs()
	for b.Loop() {
		_ = ryn.UsageFrame(u)
	}
}

// ─── Stream: Emit + Consume ─────────────────────────────────

func BenchmarkStreamEmitConsume(b *testing.B) {
	ctx := context.Background()
	f := ryn.TextFrame("tok")
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s, e := ryn.NewStream(64)
		go func() {
			defer e.Close()
			for range 100 {
				_ = e.Emit(ctx, f)
			}
		}()
		for s.Next(ctx) {
		}
	}
}

func BenchmarkStreamUnbuffered(b *testing.B) {
	ctx := context.Background()
	f := ryn.TextFrame("tok")
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s, e := ryn.NewStream(0)
		go func() {
			defer e.Close()
			for range 100 {
				_ = e.Emit(ctx, f)
			}
		}()
		for s.Next(ctx) {
		}
	}
}

// ─── Stream Utilities ───────────────────────────────────────

func BenchmarkCollect(b *testing.B) {
	ctx := context.Background()
	frames := make([]ryn.Frame, 500)
	for i := range frames {
		frames[i] = ryn.TextFrame("tok")
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s := ryn.StreamFromSlice(frames)
		_, _ = ryn.Collect(ctx, s)
	}
}

func BenchmarkCollectText(b *testing.B) {
	ctx := context.Background()
	frames := make([]ryn.Frame, 500)
	for i := range frames {
		frames[i] = ryn.TextFrame("hello")
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s := ryn.StreamFromSlice(frames)
		_, _ = ryn.CollectText(ctx, s)
	}
}

func BenchmarkStreamFromSlice(b *testing.B) {
	frames := make([]ryn.Frame, 100)
	for i := range frames {
		frames[i] = ryn.TextFrame("x")
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = ryn.StreamFromSlice(frames)
	}
}

// ─── Usage Accumulation ─────────────────────────────────────

func BenchmarkUsageAdd(b *testing.B) {
	other := &ryn.Usage{
		InputTokens:  50,
		OutputTokens: 100,
		TotalTokens:  150,
		Detail:       map[string]int{"cached": 10, "reasoning": 20},
	}
	b.ReportAllocs()
	for b.Loop() {
		u := ryn.Usage{}
		u.Add(other)
	}
}

func BenchmarkUsageAutoAccumulate(b *testing.B) {
	// Stream.Next silently accumulates KindUsage frames
	ctx := context.Background()
	frames := make([]ryn.Frame, 0, 110)
	for range 100 {
		frames = append(frames, ryn.TextFrame("tok"))
	}
	for range 10 {
		frames = append(frames, ryn.UsageFrame(&ryn.Usage{
			InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
		}))
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s := ryn.StreamFromSlice(frames)
		for s.Next(ctx) {
		}
		_ = s.Usage()
	}
}

// ─── Pipeline Stages ────────────────────────────────────────

func BenchmarkPipelinePassthrough(b *testing.B) {
	ctx := context.Background()
	frames := make([]ryn.Frame, 200)
	for i := range frames {
		frames[i] = ryn.TextFrame("tok")
	}
	p := pipe.New(pipe.PassThrough())
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		in := ryn.StreamFromSlice(frames)
		out := p.Run(ctx, in)
		for out.Next(ctx) {
		}
	}
}

func BenchmarkPipelineFilter(b *testing.B) {
	ctx := context.Background()
	frames := make([]ryn.Frame, 200)
	for i := range frames {
		if i%2 == 0 {
			frames[i] = ryn.TextFrame("tok")
		} else {
			frames[i] = ryn.ControlFrame(ryn.SignalFlush)
		}
	}
	p := pipe.New(pipe.TextOnly())
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		in := ryn.StreamFromSlice(frames)
		out := p.Run(ctx, in)
		for out.Next(ctx) {
		}
	}
}

func BenchmarkPipelineMap(b *testing.B) {
	ctx := context.Background()
	frames := make([]ryn.Frame, 200)
	for i := range frames {
		frames[i] = ryn.TextFrame("tok")
	}
	upper := pipe.Map(func(f ryn.Frame) ryn.Frame {
		if f.Kind == ryn.KindText {
			f.Text = "[" + f.Text + "]"
		}
		return f
	})
	p := pipe.New(upper)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		in := ryn.StreamFromSlice(frames)
		out := p.Run(ctx, in)
		for out.Next(ctx) {
		}
	}
}

func BenchmarkPipelineAccumulate(b *testing.B) {
	ctx := context.Background()
	frames := make([]ryn.Frame, 500)
	for i := range frames {
		frames[i] = ryn.TextFrame("a")
	}
	p := pipe.New(pipe.Accumulate())
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		in := ryn.StreamFromSlice(frames)
		out := p.Run(ctx, in)
		for out.Next(ctx) {
		}
	}
}

func BenchmarkPipelineThreeStages(b *testing.B) {
	ctx := context.Background()
	frames := make([]ryn.Frame, 200)
	for i := range frames {
		frames[i] = ryn.TextFrame("tok")
	}
	p := pipe.New(
		pipe.PassThrough(),
		pipe.Map(func(f ryn.Frame) ryn.Frame { return f }),
		pipe.PassThrough(),
	)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		in := ryn.StreamFromSlice(frames)
		out := p.Run(ctx, in)
		for out.Next(ctx) {
		}
	}
}

// ─── Forward ────────────────────────────────────────────────

func BenchmarkForward(b *testing.B) {
	ctx := context.Background()
	frames := make([]ryn.Frame, 200)
	for i := range frames {
		frames[i] = ryn.TextFrame("tok")
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		in := ryn.StreamFromSlice(frames)
		out, e := ryn.NewStream(64)
		go func() {
			defer e.Close()
			_ = ryn.Forward(ctx, in, e)
		}()
		for out.Next(ctx) {
		}
	}
}

// ─── Orchestration ──────────────────────────────────────────

func BenchmarkFan(b *testing.B) {
	ctx := context.Background()
	makeGen := func(n int) func(context.Context) (*ryn.Stream, error) {
		return func(_ context.Context) (*ryn.Stream, error) {
			frames := make([]ryn.Frame, n)
			for i := range frames {
				frames[i] = ryn.TextFrame("t")
			}
			return ryn.StreamFromSlice(frames), nil
		}
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s := orchestrate.Fan(ctx, makeGen(50), makeGen(50), makeGen(50))
		for s.Next(ctx) {
		}
	}
}

func BenchmarkRace(b *testing.B) {
	ctx := context.Background()
	makeGen := func(n int) func(context.Context) (*ryn.Stream, error) {
		return func(_ context.Context) (*ryn.Stream, error) {
			frames := make([]ryn.Frame, n)
			for i := range frames {
				frames[i] = ryn.TextFrame("t")
			}
			return ryn.StreamFromSlice(frames), nil
		}
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_, _, _ = orchestrate.Race(ctx, makeGen(20), makeGen(20), makeGen(20))
	}
}

func BenchmarkSequence(b *testing.B) {
	ctx := context.Background()
	step := func(_ context.Context, input string) (*ryn.Stream, error) {
		frames := []ryn.Frame{ryn.TextFrame(input + " step")}
		return ryn.StreamFromSlice(frames), nil
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s, _ := orchestrate.Sequence(ctx, step, step, step)
		for s.Next(ctx) {
		}
	}
}

// ─── Message Construction ───────────────────────────────────

func BenchmarkUserText(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = ryn.UserText("hello world")
	}
}

func BenchmarkMultiPartMessage(b *testing.B) {
	img := make([]byte, 1024)
	b.ReportAllocs()
	for b.Loop() {
		_ = ryn.Multi(ryn.RoleUser,
			ryn.TextPart("describe this image"),
			ryn.ImagePart(img, "image/png"),
		)
	}
}

func BenchmarkEffectiveMessages(b *testing.B) {
	req := &ryn.Request{
		SystemPrompt: "You are helpful.",
		Messages: []ryn.Message{
			ryn.UserText("hello"),
			ryn.AssistantText("hi"),
			ryn.UserText("how are you?"),
		},
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = req.EffectiveMessages()
	}
}

// ─── Hook Composition ───────────────────────────────────────

func BenchmarkHooksComposite(b *testing.B) {
	h := hook.Compose(hook.NoOpHook{}, hook.NoOpHook{}, hook.NoOpHook{})
	ctx := context.Background()
	f := ryn.TextFrame("tok")
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = h.OnFrame(ctx, f, 0)
	}
}

func BenchmarkHookOnGenerateStart(b *testing.B) {
	h := hook.Compose(hook.NoOpHook{}, hook.NoOpHook{})
	ctx := context.Background()
	info := hook.GenerateStartInfo{Model: "test", Messages: 3}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = h.OnGenerateStart(ctx, info)
	}
}

// ─── Runtime (with mock provider) ───────────────────────────

func BenchmarkRuntimeGenerate(b *testing.B) {
	ctx := context.Background()
	mock := ryn.ProviderFunc(func(_ context.Context, _ *ryn.Request) (*ryn.Stream, error) {
		frames := make([]ryn.Frame, 50)
		for i := range frames {
			frames[i] = ryn.TextFrame("tok")
		}
		s := ryn.StreamFromSlice(frames)
		return s, nil
	})
	rt := runtime.New(mock)
	req := &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s, _ := rt.Generate(ctx, req)
		for s.Next(ctx) {
		}
	}
}

func BenchmarkRuntimeWithHook(b *testing.B) {
	ctx := context.Background()
	mock := ryn.ProviderFunc(func(_ context.Context, _ *ryn.Request) (*ryn.Stream, error) {
		frames := make([]ryn.Frame, 50)
		for i := range frames {
			frames[i] = ryn.TextFrame("tok")
		}
		return ryn.StreamFromSlice(frames), nil
	})
	rt := runtime.New(mock).WithHook(hook.NoOpHook{})
	req := &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s, _ := rt.Generate(ctx, req)
		for s.Next(ctx) {
		}
	}
}

func BenchmarkRuntimeWithPipeline(b *testing.B) {
	ctx := context.Background()
	mock := ryn.ProviderFunc(func(_ context.Context, _ *ryn.Request) (*ryn.Stream, error) {
		frames := make([]ryn.Frame, 50)
		for i := range frames {
			frames[i] = ryn.TextFrame("tok")
		}
		return ryn.StreamFromSlice(frames), nil
	})
	p := pipe.New(pipe.PassThrough(), pipe.Map(func(f ryn.Frame) ryn.Frame { return f }))
	rt := runtime.New(mock).WithPipeline(p)
	req := &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s, _ := rt.Generate(ctx, req)
		for s.Next(ctx) {
		}
	}
}

func BenchmarkRuntimeFullStack(b *testing.B) {
	ctx := context.Background()
	mock := ryn.ProviderFunc(func(_ context.Context, _ *ryn.Request) (*ryn.Stream, error) {
		frames := make([]ryn.Frame, 50)
		for i := range frames {
			frames[i] = ryn.TextFrame("tok")
		}
		frames = append(frames, ryn.UsageFrame(&ryn.Usage{InputTokens: 10, OutputTokens: 50, TotalTokens: 60}))
		return ryn.StreamFromSlice(frames), nil
	})
	p := pipe.New(pipe.PassThrough())
	rt := runtime.New(mock).WithHook(hook.NoOpHook{}).WithPipeline(p)
	req := &ryn.Request{
		SystemPrompt: "You are helpful.",
		Messages:     []ryn.Message{ryn.UserText("hi")},
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s, _ := rt.Generate(ctx, req)
		for s.Next(ctx) {
		}
	}
}

// ─── BytePool ───────────────────────────────────────────────

func BenchmarkBytePoolSmall(b *testing.B) {
	pool := ryn.NewBytePool()
	b.ReportAllocs()
	for b.Loop() {
		buf := pool.Get(960) // typical 20ms PCM audio chunk
		pool.Put(buf)
	}
}

func BenchmarkBytePoolMedium(b *testing.B) {
	pool := ryn.NewBytePool()
	b.ReportAllocs()
	for b.Loop() {
		buf := pool.Get(32 * 1024) // 32KB image tile
		pool.Put(buf)
	}
}

func BenchmarkBytePoolLarge(b *testing.B) {
	pool := ryn.NewBytePool()
	b.ReportAllocs()
	for b.Loop() {
		buf := pool.Get(512 * 1024) // 512KB video frame
		pool.Put(buf)
	}
}

func BenchmarkBytePoolParallel(b *testing.B) {
	pool := ryn.NewBytePool()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			buf := pool.Get(4096)
			pool.Put(buf)
		}
	})
}

func BenchmarkPooledAudioFrame(b *testing.B) {
	pool := ryn.NewBytePool()
	data := make([]byte, 960)
	b.ReportAllocs()
	for b.Loop() {
		f := ryn.AudioFramePooled(pool, data, "audio/pcm")
		pool.Put(f.Data)
	}
}

// ─── Usage/ResponseMeta Pools ───────────────────────────────

func BenchmarkUsagePool(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		u := ryn.GetUsage()
		u.InputTokens = 100
		ryn.PutUsage(u)
	}
}

func BenchmarkResponseMetaPool(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		m := ryn.GetResponseMeta()
		m.Model = "gpt-4o"
		ryn.PutResponseMeta(m)
	}
}

// ─── Cache ──────────────────────────────────────────────────

func BenchmarkCacheHit(b *testing.B) {
	ctx := context.Background()
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("cached")}), nil
	})
	c := middleware.NewCache(middleware.CacheOptions{MaxEntries: 10000})
	provider := c.Wrap(mock)
	req := &ryn.Request{Model: "bench", Messages: []ryn.Message{ryn.UserText("hello")}}

	// Prime cache
	s, _ := provider.Generate(ctx, req)
	ryn.CollectText(ctx, s)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s, _ := provider.Generate(ctx, req)
		ryn.CollectText(ctx, s)
	}
}

func BenchmarkCacheHitParallel(b *testing.B) {
	ctx := context.Background()
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("cached")}), nil
	})
	c := middleware.NewCache(middleware.CacheOptions{MaxEntries: 10000})
	provider := c.Wrap(mock)
	req := &ryn.Request{Model: "bench", Messages: []ryn.Message{ryn.UserText("hello")}}

	// Prime cache
	s, _ := provider.Generate(ctx, req)
	ryn.CollectText(ctx, s)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s, _ := provider.Generate(ctx, req)
			ryn.CollectText(ctx, s)
		}
	})
}

// ─── Registry ───────────────────────────────────────────────

func BenchmarkRegistryGet(b *testing.B) {
	reg := registry.New()
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, nil
	})
	reg.Register("openai", mock)
	b.ReportAllocs()
	for b.Loop() {
		reg.Get("openai")
	}
}

func BenchmarkRegistryGetParallel(b *testing.B) {
	reg := registry.New()
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, nil
	})
	reg.Register("openai", mock)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			reg.Get("openai")
		}
	})
}

// ─── Structured Output ─────────────────────────────────────

func BenchmarkGenerateStructured(b *testing.B) {
	ctx := context.Background()
	type out struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"]}`)
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame(`{"name":"alice","age":30}`)}), nil
	})
	req := &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}}

	b.ReportAllocs()
	for b.Loop() {
		_, _, _, _ = structured.GenerateStructured[out](ctx, mock, req, schema)
	}
}

func BenchmarkStreamStructured(b *testing.B) {
	ctx := context.Background()
	type out struct {
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
	req := &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}}

	b.ReportAllocs()
	for b.Loop() {
		ss, _ := structured.StreamStructured[out](ctx, mock, req, schema)
		for ss.Next(ctx) {
		}
	}
}
