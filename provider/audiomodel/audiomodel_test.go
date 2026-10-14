package audiomodel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

func TestSpeechAndEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/vad":
			if body["sample_rate"] != float64(16000) || body["audio"] == "" {
				t.Fatalf("vad body %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]float32{"speech": 0.9})
		case "/turn":
			if body["text"] != "hello" {
				t.Fatalf("turn body %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]float32{"end": 0.91})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	vad := &Speech{URL: srv.URL + "/vad", On: 0.5, Off: 0.35, Silence: 20 * time.Millisecond, Rate: 16000}
	marks, err := vad.Push(context.Background(), make([]byte, 320))
	if err != nil || len(marks) != 1 || marks[0].Kind != niro.SpeechStart || marks[0].Probability != 0.9 {
		t.Fatalf("start %v %v", marks, err)
	}
	score, err := Endpoint{URL: srv.URL + "/turn"}.Predict(context.Background(), niro.TurnInput{
		Text: "hello", Silence: 240 * time.Millisecond,
	})
	if err != nil || score.End != 0.91 {
		t.Fatalf("%+v %v", score, err)
	}
}
