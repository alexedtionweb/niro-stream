package niro_test

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
		_ = niro.TextFrame("token")
	}
}

func BenchmarkAudioFrame(b *testing.B) {
	data := make([]byte, 960) // 20ms 48kHz mono PCM
	b.ReportAllocs()
	for b.Loop() {
		_ = niro.AudioFrame(data, "audio/pcm")
	}
}

func BenchmarkToolCallFrame(b *testing.B) {
	tc := &niro.ToolCall{ID: "call_1", Name: "get_weather", Args: json.RawMessage(`{"city":"NYC"}`)}
	b.ReportAllocs()
	for b.Loop() {
		_ = niro.ToolCallFrame(tc)
	}
}

func BenchmarkUsageFrame(b *testing.B) {
	u := &niro.Usage{InputTokens: 100, OutputTokens: 200, TotalTokens: 300}
	b.ReportAllocs()
	for b.Loop() {
		_ = niro.UsageFrame(u)
	}
}

// ─── Stream: Emit + Consume ─────────────────────────────────

func BenchmarkStreamEmitConsume(b *testing.B) {
	ctx := context.Background()
	f := niro.TextFrame("tok")
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s, e := niro.NewStream(64)
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
	f := niro.TextFrame("tok")
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s, e := niro.NewStream(0)
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
	frames := make([]niro.Frame, 500)
	for i := range frames {
		frames[i] = niro.TextFrame("tok")
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s := niro.StreamFromSlice(frames)
		_, _ = niro.Collect(ctx, s)
	}
}

func BenchmarkCollectText(b *testing.B) {
	ctx := context.Background()
	frames := make([]niro.Frame, 500)
	for i := range frames {
		frames[i] = niro.TextFrame("hello")
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s := niro.StreamFromSlice(frames)
		_, _ = niro.CollectText(ctx, s)
	}
}

func BenchmarkStreamFromSlice(b *testing.B) {
	frames := make([]niro.Frame, 100)
	for i := range frames {
		frames[i] = niro.TextFrame("x")
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = niro.StreamFromSlice(frames)
	}
}

// ─── Usage Accumulation ─────────────────────────────────────

func BenchmarkUsageAdd(b *testing.B) {
	other := &niro.Usage{
		InputTokens:  50,
		OutputTokens: 100,
		TotalTokens:  150,
		Detail:       map[string]int{"cached": 10, "reasoning": 20},
	}
	b.ReportAllocs()
	for b.Loop() {
		u := niro.Usage{}
		u.Add(other)
	}
}

func BenchmarkUsageAutoAccumulate(b *testing.B) {
	// Stream.Next silently accumulates KindUsage frames
	ctx := context.Background()
	frames := make([]niro.Frame, 0, 110)
	for range 100 {
		frames = append(frames, niro.TextFrame("tok"))
	}
	for range 10 {
		frames = append(frames, niro.UsageFrame(&niro.Usage{
			InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
		}))
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		s := niro.StreamFromSlice(frames)
		for s.Next(ctx) {
		}
		_ = s.Usage()
	}
}

// ─── Pipeline Stages ────────────────────────────────────────

func BenchmarkPipelinePassthrough(b *testing.B) {
	ctx := context.Background()
	frames := make([]niro.Frame, 200)
	for i := range frames {
		frames[i] = niro.TextFrame("tok")
	}
	p := pipe.New(pipe.PassThrough())
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		in := niro.StreamFromSlice(frames)
		out := p.Run(ctx, in)
		for out.Next(ctx) {
		}
	}
}

func BenchmarkPipelineFilter(b *testing.B) {
	ctx := context.Background()
	frames := make([]niro.Frame, 200)
	for i := range frames {
		if i%2 == 0 {
			frames[i] = niro.TextFrame("tok")
		} else {
			frames[i] = niro.ControlFrame(niro.SignalFlush)
		}
	}
	p := pipe.New(pipe.TextOnly())
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		in := niro.StreamFromSlice(frames)
		out := p.Run(ctx, in)
		for out.Next(ctx) {
		}
	}
}

func BenchmarkPipelineMap(b *testing.B) {
	ctx := context.Background()
	frames := make([]niro.Frame, 200)
	for i := range frames {
		frames[i] = niro.TextFrame("tok")
	}
	upper := pipe.Map(func(f niro.Frame) niro.Frame {
		if f.Kind == niro.KindText {
			f.Text = "[" + f.Text + "]"
		}
		return f
	})
	p := pipe.New(upper)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		in := niro.StreamFromSlice(frames)
		out := p.Run(ctx, in)
		for out.Next(ctx) {
		}
	}
}

func BenchmarkPipelineAccumulate(b *testing.B) {
	ctx := context.Background()
	frames := make([]niro.Frame, 500)
	for i := range frames {
		frames[i] = niro.TextFrame("a")
	}
	p := pipe.New(pipe.Accumulate())
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		in := niro.StreamFromSlice(frames)
		out := p.Run(ctx, in)
		for out.Next(ctx) {
		}
	}
}

func BenchmarkPipelineThreeStages(b *testing.B) {
	ctx := context.Background()
	frames := make([]niro.Frame, 200)
	for i := range frames {
		frames[i] = niro.TextFrame("tok")
	}
	p := pipe.New(
		pipe.PassThrough(),
		pipe.Map(func(f niro.Frame) niro.Frame { return f }),
		pipe.PassThrough(),
	)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		in := niro.StreamFromSlice(frames)
		out := p.Run(ctx, in)
		for out.Next(ctx) {
		}
	}
}

// ─── Forward ────────────────────────────────────────────────

func BenchmarkForward(b *testing.B) {
	ctx := context.Background()
	frames := make([]niro.Frame, 200)
	for i := range frames {
		frames[i] = niro.TextFrame("tok")
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		in := niro.StreamFromSlice(frames)
		out, e := niro.NewStream(64)
		go func() {
			defer e.Close()
			_ = niro.Forward(ctx, in, e)
		}()
		for out.Next(ctx) {
		}
	}
}

// ─── Orchestration ──────────────────────────────────────────

func BenchmarkFan(b *testing.B) {
	ctx := context.Background()
	makeGen := func(n int) func(context.Context) (*niro.Stream, error) {
		return func(_ context.Context) (*niro.Stream, error) {
			frames := make([]niro.Frame, n)
			for i := range frames {
				frames[i] = niro.TextFrame("t")
			}
			return niro.StreamFromSlice(frames), nil
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
	makeGen := func(n int) func(context.Context) (*niro.Stream, error) {
		return func(_ context.Context) (*niro.Stream, error) {
			frames := make([]niro.Frame, n)
			for i := range frames {
				frames[i] = niro.TextFrame("t")
			}
			return niro.StreamFromSlice(frames), nil
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
	step := func(_ context.Context, input string) (*niro.Stream, error) {
		frames := []niro.Frame{niro.TextFrame(input + " step")}
		return niro.StreamFromSlice(frames), nil
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
		_ = niro.UserText("hello world")
	}
}

func BenchmarkMultiPartMessage(b *testing.B) {
	img := make([]byte, 1024)
	b.ReportAllocs()
	for b.Loop() {
		_ = niro.Multi(niro.RoleUser,
			niro.TextPart("describe this image"),
			niro.ImagePart(img, "image/png"),
		)
	}
}

func BenchmarkEffectiveMessages(b *testing.B) {
	req := &niro.Request{
		SystemPrompt: "You are helpful.",
		Messages: []niro.Message{
			niro.UserText("hello"),
			niro.AssistantText("hi"),
			niro.UserText("how are you?"),
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
	f := niro.TextFrame("tok")
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
	mock := niro.ProviderFunc(func(_ context.Context, _ *niro.Request) (*niro.Stream, error) {
		frames := make([]niro.Frame, 50)
		for i := range frames {
			frames[i] = niro.TextFrame("tok")
		}
		s := niro.StreamFromSlice(frames)
		return s, nil
	})
	rt := runtime.New(mock)
	req := &niro.Request{Messages: []niro.Message{niro.UserText("hi")}}
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
	mock := niro.ProviderFunc(func(_ context.Context, _ *niro.Request) (*niro.Stream, error) {
		frames := make([]niro.Frame, 50)
		for i := range frames {
			frames[i] = niro.TextFrame("tok")
		}
		return niro.StreamFromSlice(frames), nil
	})
	rt := runtime.New(mock).WithHook(hook.NoOpHook{})
	req := &niro.Request{Messages: []niro.Message{niro.UserText("hi")}}
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
	mock := niro.ProviderFunc(func(_ context.Context, _ *niro.Request) (*niro.Stream, error) {
		frames := make([]niro.Frame, 50)
		for i := range frames {
			frames[i] = niro.TextFrame("tok")
		}
		return niro.StreamFromSlice(frames), nil
	})
	p := pipe.New(pipe.PassThrough(), pipe.Map(func(f niro.Frame) niro.Frame { return f }))
	rt := runtime.New(mock).WithPipeline(p)
	req := &niro.Request{Messages: []niro.Message{niro.UserText("hi")}}
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
	mock := niro.ProviderFunc(func(_ context.Context, _ *niro.Request) (*niro.Stream, error) {
		frames := make([]niro.Frame, 50)
		for i := range frames {
			frames[i] = niro.TextFrame("tok")
		}
		frames = append(frames, niro.UsageFrame(&niro.Usage{InputTokens: 10, OutputTokens: 50, TotalTokens: 60}))
		return niro.StreamFromSlice(frames), nil
	})
	p := pipe.New(pipe.PassThrough())
	rt := runtime.New(mock).WithHook(hook.NoOpHook{}).WithPipeline(p)
	req := &niro.Request{
		SystemPrompt: "You are helpful.",
		Messages:     []niro.Message{niro.UserText("hi")},
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
	pool := niro.NewBytePool()
	b.ReportAllocs()
	for b.Loop() {
		buf := pool.Get(960) // typical 20ms PCM audio chunk
		pool.Put(buf)
	}
}

func BenchmarkBytePoolMedium(b *testing.B) {
	pool := niro.NewBytePool()
	b.ReportAllocs()
	for b.Loop() {
		buf := pool.Get(32 * 1024) // 32KB image tile
		pool.Put(buf)
	}
}

func BenchmarkBytePoolLarge(b *testing.B) {
	pool := niro.NewBytePool()
	b.ReportAllocs()
	for b.Loop() {
		buf := pool.Get(512 * 1024) // 512KB video frame
		pool.Put(buf)
	}
}

func BenchmarkBytePoolParallel(b *testing.B) {
	pool := niro.NewBytePool()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			buf := pool.Get(4096)
			pool.Put(buf)
		}
	})
}

func BenchmarkPooledAudioFrame(b *testing.B) {
	pool := niro.NewBytePool()
	data := make([]byte, 960)
	b.ReportAllocs()
	for b.Loop() {
		f := niro.AudioFramePooled(pool, data, "audio/pcm")
		pool.Put(f.Data)
	}
}

// ─── Usage/ResponseMeta Pools ───────────────────────────────

func BenchmarkUsagePool(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		u := niro.GetUsage()
		u.InputTokens = 100
		niro.PutUsage(u)
	}
}

func BenchmarkResponseMetaPool(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		m := niro.GetResponseMeta()
		m.Model = "gpt-4o"
		niro.PutResponseMeta(m)
	}
}

// ─── Cache ──────────────────────────────────────────────────

func BenchmarkCacheHit(b *testing.B) {
	ctx := context.Background()
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("cached")}), nil
	})
	c := middleware.NewCache(middleware.CacheOptions{MaxEntries: 10000})
	provider := c.Wrap(mock)
	req := &niro.Request{Model: "bench", Messages: []niro.Message{niro.UserText("hello")}}

	// Prime cache
	s, _ := provider.Generate(ctx, req)
	niro.CollectText(ctx, s)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s, _ := provider.Generate(ctx, req)
		niro.CollectText(ctx, s)
	}
}

func BenchmarkCacheHitParallel(b *testing.B) {
	ctx := context.Background()
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("cached")}), nil
	})
	c := middleware.NewCache(middleware.CacheOptions{MaxEntries: 10000})
	provider := c.Wrap(mock)
	req := &niro.Request{Model: "bench", Messages: []niro.Message{niro.UserText("hello")}}

	// Prime cache
	s, _ := provider.Generate(ctx, req)
	niro.CollectText(ctx, s)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s, _ := provider.Generate(ctx, req)
			niro.CollectText(ctx, s)
		}
	})
}

// ─── Registry ───────────────────────────────────────────────

func BenchmarkRegistryGet(b *testing.B) {
	reg := registry.New()
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
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
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
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
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame(`{"name":"alice","age":30}`)}), nil
	})
	req := &niro.Request{Messages: []niro.Message{niro.UserText("hi")}}

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
	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s, e := niro.NewStream(0)
		go func() {
			defer e.Close()
			_ = e.Emit(ctx, niro.TextFrame(`{"name":"al`))
			_ = e.Emit(ctx, niro.TextFrame(`ice","age":30}`))
		}()
		return s, nil
	})
	req := &niro.Request{Messages: []niro.Message{niro.UserText("hi")}}

	b.ReportAllocs()
	for b.Loop() {
		ss, _ := structured.StreamStructured[out](ctx, mock, req, schema)
		for ss.Next(ctx) {
		}
	}
}
