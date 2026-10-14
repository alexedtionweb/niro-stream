package vllmomni

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexedtionweb/niro-stream"
)

func TestGenerateMediaAndStream(t *testing.T) {
	t.Parallel()
	pcm := []byte{0x10, 0x20, 0x30, 0x40}
	var gotAudioURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		msgs := body["messages"].([]any)
		user := msgs[0].(map[string]any)
		for _, p := range user["content"].([]any) {
			part := p.(map[string]any)
			if part["type"] == "audio_url" {
				gotAudioURL = part["audio_url"].(map[string]any)["url"].(string)
			}
			if part["type"] == "video_url" {
				if part["video_url"].(map[string]any)["url"] != "https://example.com/v.mp4" {
					t.Errorf("video %v", part["video_url"])
				}
			}
		}
		if body["modalities"].([]any)[0] != "text" {
			t.Errorf("default modalities %v", body["modalities"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		b64 := base64.StdEncoding.EncodeToString(pcm)
		_, _ = w.Write([]byte("data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n"))
		f.Flush()
		_, _ = w.Write([]byte("data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"delta\":{\"audio\":{\"data\":\"" + b64 + "\"}},\"finish_reason\":\"stop\",\"audio_metadata\":{\"format\":\"pcm16\",\"sample_rate_hz\":16000,\"channels\":1}}]}\n\n"))
		f.Flush()
		_, _ = w.Write([]byte("data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n"))
		f.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		f.Flush()
	}))
	defer srv.Close()

	llm := New(srv.URL+"/v1", "", WithModel("m"))
	stream, err := llm.Generate(context.Background(), &niro.Request{
		Model: "m",
		Messages: []niro.Message{niro.Multi(niro.RoleUser,
			niro.TextPart("go"),
			niro.Part{Kind: niro.KindAudio, URL: "https://cdn.example/a.wav", Mime: niro.AudioWAV},
			niro.Part{Kind: niro.KindVideo, URL: "https://example.com/v.mp4", Mime: "video/mp4"},
		)},
		Extra: Params{Modalities: []string{"text"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var audio []byte
	var mime string
	for stream.Next(context.Background()) {
		f := stream.Frame()
		switch f.Kind {
		case niro.KindText:
			text += f.Text
		case niro.KindAudio:
			audio = append(audio, f.Data...)
			mime = f.Mime
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if text != "Hi" {
		t.Fatalf("text %q", text)
	}
	if string(audio) != string(pcm) {
		t.Fatalf("audio %v", audio)
	}
	if mime != niro.AudioPCM16k {
		t.Fatalf("mime %s", mime)
	}
	if gotAudioURL != "https://cdn.example/a.wav" {
		t.Fatalf("audio url %s", gotAudioURL)
	}
	u := stream.Usage()
	if u.InputTokens != 1 || u.OutputTokens != 2 {
		t.Fatalf("usage %+v", u)
	}
	if stream.Response().FinishReason != "stop" {
		t.Fatalf("finish %s", stream.Response().FinishReason)
	}
}

func TestChatEndpoint(t *testing.T) {
	if chatEndpoint("http://127.0.0.1:8000") != "http://127.0.0.1:8000/v1/chat/completions" {
		t.Fatal(chatEndpoint("http://127.0.0.1:8000"))
	}
	if chatEndpoint("http://127.0.0.1:8000/v1/") != "http://127.0.0.1:8000/v1/chat/completions" {
		t.Fatal(chatEndpoint("http://127.0.0.1:8000/v1/"))
	}
	if chatEndpoint("http://127.0.0.1:8000/v1/chat/completions") != "http://127.0.0.1:8000/v1/chat/completions" {
		t.Fatal("full url")
	}
}

func TestTextOnlyDefaultSkipsAudioModalities(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mods := body["modalities"].([]any)
		if len(mods) != 1 || mods[0] != "text" {
			t.Errorf("modalities %v", mods)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	llm := New(srv.URL+"/v1", "")
	stream, err := llm.Generate(context.Background(), &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := niro.CollectText(context.Background(), stream)
	if err != nil {
		t.Fatal(err)
	}
	if text != "ok" {
		t.Fatalf("text %q", text)
	}
}

func TestToolResultCarriesVideo(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		msg := body["messages"].([]any)[0].(map[string]any)
		if msg["role"] != "tool" || msg["tool_call_id"] != "c1" {
			t.Fatalf("msg %#v", msg)
		}
		var sawVideo bool
		for _, p := range msg["content"].([]any) {
			part := p.(map[string]any)
			if part["type"] == "video_url" && part["video_url"].(map[string]any)["url"] == "https://cam.example/now.mp4" {
				sawVideo = true
			}
		}
		if !sawVideo {
			t.Fatalf("content %#v", msg["content"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"seen\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	msg := niro.ToolMessage("c1", "ok")
	msg.Parts[0].Result.Parts = []niro.Part{{
		Kind: niro.KindVideo,
		URL:  "https://cam.example/now.mp4",
		Mime: "video/mp4",
	}}
	llm := New(srv.URL+"/v1", "")
	stream, err := llm.Generate(context.Background(), &niro.Request{Messages: []niro.Message{msg}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := niro.CollectText(context.Background(), stream)
	if err != nil || text != "seen" {
		t.Fatalf("text %q err %v", text, err)
	}
}
