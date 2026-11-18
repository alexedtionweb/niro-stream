package openai_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
	. "github.com/alexedtionweb/niro-stream/provider/openai"
)

func TestDecide_Native(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/decisions" || r.Method != http.MethodPost {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if req["model"] != "gpt-6-luna" {
			t.Fatalf("model = %#v", req["model"])
		}
		if req["input"] != "charged twice" {
			t.Fatalf("input = %#v", req["input"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "dec_oai",
			"model": "gpt-6-luna",
			"answers": []map[string]any{
				{"type": "predicate", "name": "is_billing", "probability": 0.88},
				{"type": "choice", "name": "dept", "choice": "billing", "confidence": 0.9},
			},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 3},
		})
	}))
	t.Cleanup(srv.Close)

	p := New("test-key",
		WithBaseURL(srv.URL+"/v1"),
		WithHTTPClient(srv.Client()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := p.Decide(ctx, &niro.DecisionRequest{
		State: "charged twice",
		Questions: map[string]niro.DecisionQuestion{
			"is_billing": niro.Noul("Is this a billing issue?", "payment", "product"),
			"dept": niro.Choice("Department?", map[string]string{
				"billing":   "payments",
				"technical": "product",
			}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "dec_oai" {
		t.Fatalf("id = %q", res.ID)
	}
	if !res.Answers["is_billing"].True() {
		t.Fatalf("is_billing = %+v", res.Answers["is_billing"])
	}
	if res.Answers["dept"].Choice != "billing" {
		t.Fatalf("dept = %+v", res.Answers["dept"])
	}
}

func TestDecide_ChatFallback(t *testing.T) {
	body := sseBody(
		textChunk("c1", "gpt-4o-mini", `{"answers":{"ok":{"type":"noul","noul":0.7}}}`, ""),
		usageChunk("c1", "gpt-4o-mini", 5, 4, 9),
	)
	p := New("test-key",
		WithModel("gpt-4o-mini"),
		WithRequestOption(mockMiddleware(func(r *http.Request) (int, string, string) {
			if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
				t.Fatalf("path = %s", r.URL.Path)
			}
			return 200, body, "text/event-stream"
		})),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := p.Decide(ctx, &niro.DecisionRequest{
		Model: "gpt-4o-mini",
		State: "something",
		Questions: map[string]niro.DecisionQuestion{
			"ok": niro.Noul("Is this okay?", "", ""),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Answers["ok"].True() {
		t.Fatalf("ok = %+v", res.Answers["ok"])
	}
}
