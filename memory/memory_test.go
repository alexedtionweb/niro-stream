package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

func TestPutRecallForget(t *testing.T) {
	m := New(2)
	ctx := context.Background()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	first, err := m.Put(ctx, Record{Scope: "room", Source: "user", Body: "one", At: at})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Put(ctx, Record{ID: first.ID, Scope: "room", Source: "user", Body: "replaced", At: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Put(ctx, Record{
		Scope: "room", Source: "capture", Body: "door", Ref: "frame:1",
		Tags: []string{"live", "camera"}, Meta: map[string]string{"cam": "a"}, At: at.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Put(ctx, Record{Scope: "room", Source: "tool", Body: "three", At: at.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Put(ctx, Record{Scope: "other", Source: "tool", Body: "elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Put(ctx, Record{}); err == nil {
		t.Fatal("empty record")
	}

	got, err := m.Recall(ctx, Query{Scope: "room"})
	if err != nil || len(got) != 2 || got[0].Body != "door" || got[1].Body != "three" {
		t.Fatalf("%#v %v", got, err)
	}
	got[0].Tags[0] = "mutated"
	again, _ := m.Recall(ctx, Query{Scope: "room", Sources: []string{"capture"}, Tags: []string{"live", "camera"}})
	if len(again) != 1 || again[0].Tags[0] != "live" || again[0].Meta["cam"] != "a" {
		t.Fatalf("%#v", again)
	}
	window, _ := m.Recall(ctx, Query{Scope: "room", Since: at.Add(time.Second), Until: at.Add(2 * time.Second)})
	if len(window) != 1 || window[0].Source != "capture" {
		t.Fatalf("%#v", window)
	}
	if n, err := m.Forget(ctx, Query{AnyScope: true, Sources: []string{"tool"}}); err != nil || n != 2 {
		t.Fatalf("forget %d %v", n, err)
	}
}

func TestPipeCommit(t *testing.T) {
	m := New(0)
	ctx := context.Background()
	pipe := NewPipe(
		Filter(func(r Record) bool { return r.Source != "noise" }),
		Map(func(_ context.Context, r Record) ([]Record, error) {
			if r.Source != "capture" {
				r.Tags = append(r.Tags, "kept")
				return []Record{r}, nil
			}
			return []Record{
				{Scope: r.Scope, Source: "capture", Body: "person", Tags: []string{"vision"}},
				{Scope: r.Scope, Source: "capture", Body: "door", Tags: []string{"vision"}},
			}, nil
		}),
	).Then(Filter(func(r Record) bool { return r.Body != "door" }))

	out, err := pipe.Commit(ctx, m,
		Record{Scope: "s", Source: "noise", Body: "ignore"},
		Record{Scope: "s", Source: "user", Body: "hello"},
		Record{Scope: "s", Source: "capture", Body: "raw"},
	)
	if err != nil || len(out) != 2 {
		t.Fatalf("%#v %v", out, err)
	}
	got, _ := m.Recall(ctx, Query{Scope: "s"})
	if len(got) != 2 || got[0].Body != "hello" || got[0].Tags[0] != "kept" || got[1].Body != "person" {
		t.Fatalf("%#v", got)
	}

	boom := NewPipe(Map(func(context.Context, Record) ([]Record, error) {
		return nil, errors.New("bad")
	}))
	if _, err := boom.Commit(ctx, m, Record{Scope: "s", Source: "user", Body: "nope"}); err == nil {
		t.Fatal("want stage error")
	}
	got, _ = m.Recall(ctx, Query{Scope: "s"})
	if len(got) != 2 {
		t.Fatalf("stage error stored %#v", got)
	}
}

func TestViewSections(t *testing.T) {
	m := New(0)
	ctx := context.Background()
	_, _ = m.Put(ctx, Record{Scope: "s", Source: "capture", Body: "entered"})
	_, _ = m.Put(ctx, Record{Scope: "s", Source: "tool", Body: "sunny"})
	_, _ = m.Put(ctx, Record{Scope: "other", Source: "capture", Body: "hidden"})

	v := View{
		Base: "watch the room",
		Sections: []Section{
			{Title: "seen", Query: &Query{Sources: []string{"capture"}}},
			{Title: "tools", Query: &Query{Sources: []string{"tool"}, Limit: 1}},
			{Title: "empty", Query: &Query{Sources: []string{"missing"}}},
			{Title: "note", Write: func([]Record) string { return "stay short" }},
		},
	}
	prompt, err := v.Build(ctx, m, "s")
	if err != nil {
		t.Fatal(err)
	}
	want := "watch the room\n\nseen\ncapture: entered\n\ntools\ntool: sunny\n\nnote\nstay short"
	if prompt != want {
		t.Fatalf("prompt %q", prompt)
	}
}

func TestPlanViewsAndRace(t *testing.T) {
	m := New(0)
	ctx := context.Background()
	_, _ = m.Put(ctx, Record{Scope: "s", Source: "capture", Body: "entered"})
	_, _ = m.Put(ctx, Record{Scope: "s", Source: "tool", Body: "sunny"})
	_, _ = m.Put(ctx, Record{Scope: "other", Source: "capture", Body: "elsewhere"})

	var mu sync.Mutex
	seen := map[string]string{}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	provider := func(name, reply string) niro.Provider {
		return niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
			mu.Lock()
			seen[name] = req.SystemPrompt
			mu.Unlock()
			if name == "custom" {
				if req.Model != "picked" || len(req.Messages) != 1 {
					return nil, niro.NewError(niro.ErrCodeInvalidRequest, "request was not rewritten")
				}
			}
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			s, em := niro.NewStream(1)
			go func() {
				defer em.Close()
				_ = em.Emit(ctx, niro.TextFrame(reply))
			}()
			return s, nil
		})
	}

	capture := &View{Sections: []Section{{Query: &Query{Sources: []string{"capture"}}}}}
	plan := &Plan{
		Store:    m,
		Scope:    "s",
		View:     View{Base: "base", Sections: []Section{{Query: &Query{Sources: []string{"tool"}}}}},
		Messages: []niro.Message{niro.UserText("what changed?")},
		Targets: []Target{
			{Name: "eyes", Provider: provider("eyes", "saw"), View: capture},
			{Name: "other", Provider: provider("other", "far"), Scope: "other", View: capture},
			{
				Name: "custom", Provider: provider("custom", "rewritten"),
				Request: func(prompt string, _ []niro.Part) *niro.Request {
					return &niro.Request{Model: "picked", SystemPrompt: prompt, Messages: []niro.Message{niro.UserText("only this")}}
				},
			},
		},
	}
	type runOut struct {
		out []Result
		err error
	}
	done := make(chan runOut, 1)
	go func() {
		out, err := plan.Run(ctx)
		done <- runOut{out, err}
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("models did not start together")
		}
	}
	close(release)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	out := got.out
	if out[0].Text != "saw" || out[0].Prompt != "capture: entered" {
		t.Fatalf("eyes %#v", out[0])
	}
	if out[1].Prompt != "capture: elsewhere" || out[2].Text != "rewritten" || out[2].Model != "picked" {
		t.Fatalf("%#v", out)
	}
	if seen["custom"] != "base\n\ntool: sunny" {
		t.Fatalf("custom prompt %q", seen["custom"])
	}

	cancelled := make(chan struct{})
	entered := make(chan struct{})
	race := &Plan{
		Store: m,
		Scope: "s",
		View:  View{Base: "go"},
		Race:  true,
		Targets: []Target{
			{Name: "slow", Provider: niro.ProviderFunc(func(ctx context.Context, _ *niro.Request) (*niro.Stream, error) {
				close(entered)
				<-ctx.Done()
				close(cancelled)
				return nil, ctx.Err()
			})},
			{Name: "fast", Provider: niro.ProviderFunc(func(ctx context.Context, _ *niro.Request) (*niro.Stream, error) {
				select {
				case <-entered:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				s, em := niro.NewStream(1)
				go func() {
					defer em.Close()
					_ = em.Emit(ctx, niro.TextFrame("first"))
				}()
				return s, nil
			})},
		},
	}
	won, err := race.Run(ctx)
	if err != nil || len(won) != 1 || won[0].Text != "first" {
		t.Fatalf("%#v %v", won, err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("slow model was not cancelled")
	}
}
