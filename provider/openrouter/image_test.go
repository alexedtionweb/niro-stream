package openrouter_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
	. "github.com/alexedtionweb/niro-stream/provider/openrouter"
)

func TestGenerateImage_Buffered(t *testing.T) {
	png1x1 := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	}
	b64 := base64.StdEncoding.EncodeToString(png1x1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if req["prompt"] != "a mountain" {
			t.Fatalf("prompt = %#v", req["prompt"])
		}
		if req["model"] != "recraft/recraft-v4.1-flash-v2:free" {
			t.Fatalf("model = %#v", req["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": 1,
			"data": []map[string]any{
				{"b64_json": b64, "media_type": "image/png"},
			},
			"usage": map[string]any{
				"prompt_tokens":     3,
				"completion_tokens": 10,
				"total_tokens":      13,
				"cost":              0.007,
			},
		})
	}))
	t.Cleanup(srv.Close)

	p := New("test-key",
		WithServerURL(srv.URL+"/v1"),
		WithClient(srv.Client()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := p.GenerateImage(ctx, &niro.ImageRequest{
		Prompt: "a mountain",
	})
	if err != nil {
		t.Fatal(err)
	}

	var images int
	for stream.Next(ctx) {
		f := stream.Frame()
		if f.Kind != niro.KindImage {
			continue
		}
		images++
		if f.Mime != "image/png" {
			t.Fatalf("mime = %q", f.Mime)
		}
		if string(f.Data) != string(png1x1) {
			t.Fatalf("decoded bytes mismatch")
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if images != 1 {
		t.Fatalf("images = %d", images)
	}
	usage := stream.Usage()
	if usage.TotalTokens != 13 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.Detail[niro.UsageCostMicros] != 7000 {
		t.Fatalf("cost_micros = %d", usage.Detail[niro.UsageCostMicros])
	}
}

func TestGenerateImage_EmptyPrompt(t *testing.T) {
	p := New("key")
	_, err := p.GenerateImage(context.Background(), &niro.ImageRequest{})
	if err == nil || !strings.Contains(err.Error(), "empty prompt") {
		t.Fatalf("got %v", err)
	}
}

func TestGenerateImage_NilRequest(t *testing.T) {
	p := New("key")
	_, err := p.GenerateImage(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error")
	}
}
