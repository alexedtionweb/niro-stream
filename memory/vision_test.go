package memory

import (
	"context"
	"testing"

	"github.com/alexedtionweb/niro-stream"
)

func TestVisionModelSeesFrames(t *testing.T) {
	ctx := context.Background()
	store := New(0)
	reel := NewReel(2)
	jpeg := []byte{0xff, 0xd8, 1}
	png := []byte{0x89, 0x50, 0x4e, 0x47}
	clip := []byte{0, 0, 0, 1}
	if err := reel.Put("frame:1", niro.ImagePart(jpeg, "image/jpeg")); err != nil {
		t.Fatal(err)
	}
	if err := reel.Put("frame:2", niro.ImagePart(png, "image/png")); err != nil {
		t.Fatal(err)
	}
	if err := reel.Put("frame:3", niro.ImagePart([]byte{3}, "image/jpeg")); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := reel.Open(ctx, "frame:1"); ok {
		t.Fatal("oldest frame should have been dropped")
	}
	jpeg[0] = 0
	opened, ok, err := reel.Open(ctx, "frame:2")
	if err != nil || !ok || opened.Data[0] != 0x89 {
		t.Fatalf("copy %v %v %v", opened.Data, ok, err)
	}

	_, _ = store.Put(ctx, Record{Scope: "room", Source: "capture", Body: "older", Ref: "frame:2"})
	_, _ = store.Put(ctx, Record{Scope: "room", Source: "capture", Body: "newer", Ref: "frame:3"})
	_, _ = store.Put(ctx, Record{Scope: "room", Source: "note", Body: "no picture"})

	var got []niro.Part
	plan := &Plan{
		Store:    store,
		Scope:    "room",
		Album:    reel,
		View:     View{Base: "say what changed"},
		Messages: []niro.Message{niro.UserText("look")},
		Targets: []Target{{
			Name: "vision",
			Provider: niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
				got = req.Messages[0].Parts
				s, em := niro.NewStream(1)
				go func() {
					defer em.Close()
					_ = em.Emit(ctx, niro.TextFrame("a door opened"))
				}()
				return s, nil
			}),
			See:   &Query{Sources: []string{"capture"}},
			Parts: []niro.Part{niro.VideoPart(clip, "video/mp4")},
		}},
	}
	out, err := plan.Run(ctx)
	if err != nil || out[0].Err != nil || out[0].Text != "a door opened" {
		t.Fatalf("%#v %v", out, err)
	}
	if len(got) != 4 || got[0].Text != "look" || string(got[1].Data) != string(png) || got[1].Mime != "image/png" {
		t.Fatalf("%#v", got)
	}
	if string(got[2].Data) != string([]byte{3}) || got[3].Kind != niro.KindVideo {
		t.Fatalf("%#v", got)
	}

	saved, err := plan.Save(ctx, out)
	if err != nil || len(saved) != 1 || saved[0].Source != "vision" || saved[0].Body != "a door opened" {
		t.Fatalf("%#v %v", saved, err)
	}
	recalled, _ := store.Recall(ctx, Query{Scope: "room", Sources: []string{"vision"}})
	if len(recalled) != 1 {
		t.Fatalf("%#v", recalled)
	}

	sawURL := false
	byURL := &Plan{
		Store: store,
		Scope: "room",
		Album: reel,
		Targets: []Target{{
			Name: "blind",
			Provider: niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
				sawURL = req.Messages[0].Parts[0].URL == "https://cam/missing"
				s, em := niro.NewStream(1)
				go func() {
					defer em.Close()
					_ = em.Emit(ctx, niro.TextFrame("url"))
				}()
				return s, nil
			}),
			See:   &Query{ID: "nope"},
			Parts: []niro.Part{niro.ImageURLPart("https://cam/missing", "image/jpeg")},
		}},
	}
	out, err = byURL.Run(ctx)
	if err != nil || out[0].Err != nil || !sawURL {
		t.Fatalf("%#v %v", out, err)
	}

	_, _ = store.Put(ctx, Record{Scope: "room", Source: "capture", Ref: "missing-frame"})
	gone := &Plan{
		Store: store,
		Scope: "room",
		Album: reel,
		Targets: []Target{{
			Name:     "gone",
			Provider: byURL.Targets[0].Provider,
			See:      &Query{Sources: []string{"capture"}},
		}},
	}
	out, err = gone.Run(ctx)
	if err != nil || out[0].Err == nil {
		t.Fatalf("want missing media, got %#v %v", out, err)
	}
}
