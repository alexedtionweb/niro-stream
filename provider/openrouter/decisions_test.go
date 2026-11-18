package openrouter_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
	. "github.com/alexedtionweb/niro-stream/provider/openrouter"
)

func TestDecide_TypedAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/alpha/decisions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if req["model"] != "typesafe/jev-1.13" {
			t.Fatalf("model = %#v", req["model"])
		}
		if req["state"] != "Checkout blank after Pay." {
			t.Fatalf("state = %#v", req["state"])
		}
		qs, ok := req["questions"].(map[string]any)
		if !ok || qs["is_bug"] == nil || qs["team"] == nil || qs["urgency"] == nil {
			t.Fatalf("questions = %#v", req["questions"])
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "dec_1",
			"model": "typesafe/jev-1.13",
			"answers": map[string]any{
				"is_bug":  map[string]any{"type": "noul", "noul": 0.91},
				"team":    map[string]any{"type": "choice", "choice": "frontend", "confidence": 0.8},
				"urgency": map[string]any{"type": "score", "score": 2},
			},
			"usage": map[string]any{
				"input_tokens":  12,
				"output_tokens": 4,
				"cost":          0.0015,
			},
		})
	}))
	t.Cleanup(srv.Close)

	p := New("test-key",
		WithServerURL(srv.URL+"/api/v1"),
		WithClient(srv.Client()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := p.Decide(ctx, &niro.DecisionRequest{
		State: "Checkout blank after Pay.",
		Questions: map[string]niro.DecisionQuestion{
			"is_bug": niro.Noul(
				"Is the customer reporting a software defect?",
				"Broken or unexpected product behavior.",
				"Question or feature request.",
			),
			"team": niro.Choice("Which team should own this?", map[string]string{
				"frontend": "Rendering or browser issues.",
				"payments": "Checkout or billing issues.",
			}),
			"urgency": niro.Score(
				"How urgent is this?",
				"Can wait for next release",
				"Should be fixed this week",
				"Blocking revenue now",
			),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "dec_1" {
		t.Fatalf("id = %q", res.ID)
	}
	if !res.Answers["is_bug"].True() {
		t.Fatalf("is_bug = %+v", res.Answers["is_bug"])
	}
	if res.Answers["team"].Choice != "frontend" {
		t.Fatalf("team = %+v", res.Answers["team"])
	}
	if res.Answers["urgency"].Score != 2 {
		t.Fatalf("urgency = %+v", res.Answers["urgency"])
	}
	if res.Usage.InputTokens != 12 || res.Usage.OutputTokens != 4 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	if res.Usage.Detail[niro.UsageCostMicros] != 1500 {
		t.Fatalf("cost_micros = %d", res.Usage.Detail[niro.UsageCostMicros])
	}
}

func TestDecide_Validation(t *testing.T) {
	p := New("test-key")
	ctx := context.Background()

	if _, err := p.Decide(ctx, nil); err == nil {
		t.Fatal("expected nil request error")
	}
	if _, err := p.Decide(ctx, &niro.DecisionRequest{}); err == nil {
		t.Fatal("expected no questions error")
	}
	if _, err := p.Decide(ctx, &niro.DecisionRequest{
		State: "x",
		Questions: map[string]niro.DecisionQuestion{
			"q": {Kind: niro.DecisionChoice, Instructions: "pick"},
		},
	}); err == nil {
		t.Fatal("expected choice criteria error")
	}
}
