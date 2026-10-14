package realtime

import (
	"testing"

	"github.com/alexedtionweb/niro-stream"
)

func TestNormalizeOmniURL(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8000":           "ws://127.0.0.1:8000/v1/realtime",
		"http://127.0.0.1:8000/v1":        "ws://127.0.0.1:8000/v1/realtime",
		"https://example.test/v1/":        "wss://example.test/v1/realtime",
		"ws://127.0.0.1:8000/v1/realtime": "ws://127.0.0.1:8000/v1/realtime",
	}
	for in, want := range cases {
		if got := normalizeOmniURL(in); got != want {
			t.Fatalf("%s -> %s want %s", in, got, want)
		}
	}
}

func TestTurnDetectionModes(t *testing.T) {
	omit, ok := turnDetection(niro.RealtimeConfig{TurnDetection: "omit"}, dialectVLLM)
	if ok || omit != nil {
		t.Fatalf("omit include=%v value=%v", ok, omit)
	}
	manual, ok := turnDetection(niro.RealtimeConfig{TurnDetection: "manual"}, dialectVLLM)
	if !ok || manual != nil {
		t.Fatalf("manual include=%v value=%v", ok, manual)
	}
	empty, ok := turnDetection(niro.RealtimeConfig{}, dialectVLLM)
	if !ok || empty != nil {
		t.Fatalf("empty include=%v value=%v", ok, empty)
	}
	got, ok := turnDetection(niro.RealtimeConfig{
		TurnDetection: "server_vad",
		VAD:           &niro.VADConfig{Threshold: 0.5, SilenceDurationMs: 400},
	}, dialectVLLM)
	if !ok {
		t.Fatal("server_vad omitted")
	}
	td := got.(map[string]any)
	if td["type"] != "server_vad" || td["silence_duration_ms"] != 400 {
		t.Fatalf("%v", td)
	}
}
