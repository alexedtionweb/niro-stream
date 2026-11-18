package compat_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/provider/compat"
)

func TestDecide_ViaChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if req["model"] != "llama3.2" {
			t.Fatalf("model = %#v", req["model"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		payload := `{"answers":{"is_bug":{"type":"noul","noul":0.81},"team":{"type":"choice","choice":"frontend"}}}`
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"llama3.2\",\"choices\":[{\"delta\":{\"content\":"+jsonString(payload)+"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"llama3.2\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":6,\"total_tokens\":14}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	p := compat.New(srv.URL+"/v1", "", compat.WithModel("llama3.2"), compat.WithClient(srv.Client()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := p.Decide(ctx, &niro.DecisionRequest{
		State: "Checkout blank after Pay.",
		Questions: map[string]niro.DecisionQuestion{
			"is_bug": niro.Noul("Is this a defect?", "broken", "question"),
			"team": niro.Choice("Owner?", map[string]string{
				"frontend": "UI",
				"payments": "billing",
			}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Answers["is_bug"].True() {
		t.Fatalf("is_bug = %+v", res.Answers["is_bug"])
	}
	if res.Answers["team"].Choice != "frontend" {
		t.Fatalf("team = %+v", res.Answers["team"])
	}
}

func TestDecide_BoxRejected(t *testing.T) {
	p := compat.New("http://example.invalid/v1", "", compat.WithModel("x"))
	_, err := p.Decide(context.Background(), &niro.DecisionRequest{
		State: "img",
		Questions: map[string]niro.DecisionQuestion{
			"btn": niro.Box("the submit button"),
		},
	})
	if err == nil {
		t.Fatal("expected box error")
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
