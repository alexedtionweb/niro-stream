package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/alexedtionweb/niro-stream"
)

func TestOpenAISessionAudioTextAndTools(t *testing.T) {
	t.Parallel()
	pcmIn := []byte{1, 2, 3, 4, 5, 6}
	pcmOut := []byte{9, 8, 7, 6}
	gotUpdate := make(chan map[string]any, 1)
	var appendRaw map[string]any

	srv := serveWS(t, func(conn *websocket.Conn) {
		_ = conn.WriteJSON(map[string]any{
			"type": "session.created",
			"session": map[string]any{
				"id":   "sess_1",
				"type": "realtime",
			},
		})
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read update: %v", err)
			return
		}
		var update map[string]any
		if json.Unmarshal(msg, &update) != nil {
			t.Errorf("update json")
			return
		}
		gotUpdate <- update
		_, msg, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read append: %v", err)
			return
		}
		_ = json.Unmarshal(msg, &appendRaw)
		b64 := base64.StdEncoding.EncodeToString(pcmOut)
		_ = conn.WriteJSON(map[string]any{
			"type":           "response.output_audio.delta",
			"delta":          b64,
			"sample_rate_hz": 24000,
		})
		_ = conn.WriteJSON(map[string]any{
			"type":  "response.output_audio_transcript.delta",
			"delta": "Hello",
		})
		_ = conn.WriteJSON(map[string]any{
			"type": "response.output_item.added",
			"item": map[string]any{"type": "function_call", "call_id": "call_1", "name": "get_weather"},
		})
		_ = conn.WriteJSON(map[string]any{
			"type":      "response.function_call_arguments.done",
			"call_id":   "call_1",
			"name":      "get_weather",
			"arguments": `{"city":"Tokyo"}`,
		})
		_ = conn.WriteJSON(map[string]any{"type": "input_audio_buffer.speech_started"})
		_ = conn.WriteJSON(map[string]any{
			"type": "response.done",
			"response": map[string]any{
				"status": "completed",
				"usage": map[string]any{
					"input_tokens":  11,
					"output_tokens": 7,
					"total_tokens":  18,
					"output_token_details": map[string]any{
						"audio_tokens": 5,
						"text_tokens":  2,
					},
				},
			},
		})
		time.Sleep(200 * time.Millisecond)
	})
	defer srv.Close()

	p := New("sk-test", WithEndpoint(wsURL(srv.URL)+"/v1/realtime"), WithHeader("X-Test", "1"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := p.Session(ctx, niro.RealtimeConfig{
		SystemPrompt: "be brief",
		Voice:        "marin",
		Tools: []niro.Tool{{
			Name:        "get_weather",
			Description: "weather",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
		VAD: &niro.VADConfig{SilenceDurationMs: 200, Threshold: 0.5},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	var update map[string]any
	select {
	case update = <-gotUpdate:
	case <-ctx.Done():
		t.Fatal("no session.update")
	}
	if update["type"] != "session.update" {
		t.Fatalf("update %#v", update)
	}
	session := update["session"].(map[string]any)
	if session["type"] != "realtime" {
		t.Fatalf("session %#v", session)
	}
	audio := session["audio"].(map[string]any)
	out := audio["output"].(map[string]any)
	if out["voice"] != "marin" {
		t.Fatalf("voice %#v", out)
	}
	if _, ok := session["voice"]; ok {
		t.Fatal("GA session must not use top-level voice")
	}

	outStream := sess.Recv()
	if err := sess.Send(ctx, niro.AudioFrame(pcmIn, niro.AudioPCM24k)); err != nil {
		t.Fatal(err)
	}

	var gotPCM []byte
	var text string
	var flushed bool
	var toolName string
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for turn")
		default:
		}
		if !outStream.Next(ctx) {
			t.Fatal(outStream.Err())
		}
		f := outStream.Frame()
		switch f.Kind {
		case niro.KindCustom:
			if f.Custom != nil && f.Custom.Type == EventSession {
				info := f.Custom.Data.(SessionInfo)
				if info.ID != "sess_1" {
					t.Fatalf("session %#v", info)
				}
			}
		case niro.KindAudio:
			gotPCM = append(gotPCM, f.Data...)
		case niro.KindText:
			text += f.Text
		case niro.KindToolCall:
			toolName = f.Tool.Name
			if string(f.Tool.Args) != `{"city":"Tokyo"}` {
				t.Fatalf("args %s", f.Tool.Args)
			}
		case niro.KindControl:
			if f.Signal == niro.SignalFlush {
				flushed = true
			}
			if f.Signal == niro.SignalEOT {
				goto done
			}
		}
	}
done:
	if string(gotPCM) != string(pcmOut) {
		t.Fatalf("pcm %v", gotPCM)
	}
	if text != "Hello" || toolName != "get_weather" || !flushed {
		t.Fatalf("text %q tool %q flush %v", text, toolName, flushed)
	}
	if outStream.Usage().OutputTokens != 7 {
		t.Fatalf("usage %+v", outStream.Usage())
	}
	b64, _ := appendRaw["audio"].(string)
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || string(raw) != string(pcmIn) {
		t.Fatalf("appended %q err %v", b64, err)
	}
	if _, ok := appendRaw["sample_rate_hz"]; ok {
		t.Fatal("OpenAI append must not carry vLLM rate fields")
	}
}

func TestOpenAIDoesNotSendBetaHeader(t *testing.T) {
	t.Parallel()
	var header string
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("OpenAI-Beta")
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"type": "session.created", "session": map[string]any{"id": "s"}})
		_, _, _ = conn.ReadMessage()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()
	p := New("sk", WithEndpoint(wsURL(srv.URL)+"/v1/realtime"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := p.Session(ctx, niro.RealtimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sess.Close()
	if header != "" {
		t.Fatalf("beta header %q", header)
	}
}

func TestVLLMOmniCommitPlaybackAndLegacyAudio(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var types []string
	srv := serveWS(t, func(conn *websocket.Conn) {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		var update map[string]any
		_ = json.Unmarshal(msg, &update)
		session := update["session"].(map[string]any)
		if session["input_audio_format"] != "pcm16" {
			t.Errorf("format %v", session["input_audio_format"])
		}
		if session["ref_audio"] != "data:audio/wav;base64,QQ" {
			t.Errorf("ref %v", session["ref_audio"])
		}
		extra := session["extra_body"].(map[string]any)
		if extra["auto_response"] != false {
			t.Errorf("auto_response %v", extra["auto_response"])
		}
		_ = conn.WriteJSON(map[string]any{
			"type":         "session.created",
			"resume_token": "rt",
			"session": map[string]any{
				"id": "duplex-1",
				"capabilities": map[string]any{
					"supports_session_resume": true,
					"supports_barge_in":       true,
					"implementation_level":    "model_native_duplex",
					"chunk_period_ms":         1000,
				},
				"audio": map[string]any{
					"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 16000}},
					"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
				},
			},
		})
		// append, commit, playback.ack — all queued before the client reads.
		for i := 0; i < 3; i++ {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("read %d: %v", i, err)
				return
			}
			var ev map[string]any
			_ = json.Unmarshal(msg, &ev)
			mu.Lock()
			types = append(types, ev["type"].(string))
			mu.Unlock()
		}
		_ = conn.WriteJSON(map[string]any{
			"type":           "response.audio.delta",
			"audio":          base64.StdEncoding.EncodeToString([]byte{1, 2}),
			"format":         "pcm16",
			"sample_rate_hz": 24000,
		})
		_ = conn.WriteJSON(map[string]any{"type": "response.listen", "response_id": "r1"})
		time.Sleep(200 * time.Millisecond)
	})
	defer srv.Close()

	p := NewOmni(srv.URL, WithHeartbeat(0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := p.Session(ctx, niro.RealtimeConfig{
		Model:       "openbmb/MiniCPM-o-4_5",
		InputFormat: niro.AudioPCM16k,
		Extra: SessionConfig{
			ModelNative: true,
			RefAudio:    "data:audio/wav;base64,QQ",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if err := sess.Send(ctx, niro.AudioFrame([]byte{3, 4}, niro.AudioPCM16k)); err != nil {
		t.Fatal(err)
	}
	if err := sess.Send(ctx, niro.ControlFrame(niro.SignalEOT)); err != nil {
		t.Fatal(err)
	}
	if err := sess.Send(ctx, niro.CustomFrame(&niro.ExperimentalFrame{
		Type: EventPlaybackAck,
		Data: PlaybackAck{PlayedMs: 40, ResponseID: "r1"},
	})); err != nil {
		t.Fatal(err)
	}

	var heardListen bool
	var audio bool
	for sess.Recv().Next(ctx) {
		f := sess.Recv().Frame()
		if f.Kind == niro.KindAudio {
			audio = true
		}
		if f.Kind == niro.KindCustom && f.Custom.Type == EventListen {
			heardListen = true
			break
		}
	}
	if !audio || !heardListen {
		t.Fatalf("audio %v listen %v", audio, heardListen)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, "input_audio_buffer.append") || !strings.Contains(joined, "input_audio_buffer.commit") {
		t.Fatalf("events %s", joined)
	}
	if strings.Contains(joined, "response.create") {
		t.Fatalf("model-native must not response.create: %s", joined)
	}
	if !strings.Contains(joined, "playback.ack") {
		t.Fatalf("events %s", joined)
	}
}

func TestTextEOTSkipsEmptyCommit(t *testing.T) {
	t.Parallel()
	var types []string
	srv := serveWS(t, func(conn *websocket.Conn) {
		_ = conn.WriteJSON(map[string]any{"type": "session.created", "session": map[string]any{"id": "s"}})
		_, _, _ = conn.ReadMessage() // session.update
		for i := 0; i < 2; i++ {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var ev map[string]any
			_ = json.Unmarshal(msg, &ev)
			types = append(types, ev["type"].(string))
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "Paris"})
		_ = conn.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}})
		time.Sleep(100 * time.Millisecond)
	})
	defer srv.Close()
	p := New("sk", WithEndpoint(wsURL(srv.URL)+"/v1/realtime"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := p.Session(ctx, niro.RealtimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Send(ctx, niro.TextFrame("capital?")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Send(ctx, niro.ControlFrame(niro.SignalEOT)); err != nil {
		t.Fatal(err)
	}
	var text string
	for sess.Recv().Next(ctx) {
		f := sess.Recv().Frame()
		if f.Kind == niro.KindText {
			text += f.Text
		}
		if f.Kind == niro.KindControl && f.Signal == niro.SignalEOT {
			break
		}
	}
	if text != "Paris" {
		t.Fatalf("text %q", text)
	}
	if strings.Join(types, ",") != "conversation.item.create,response.create" {
		t.Fatalf("events %v", types)
	}
}

func TestVLLMResumeDedupsReplay(t *testing.T) {
	t.Parallel()
	first := []byte{1, 1}
	second := []byte{2, 2}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if r.URL.Query().Get("autostart") == "0" {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("resume read: %v", err)
				return
			}
			var ev map[string]any
			_ = json.Unmarshal(msg, &ev)
			if ev["type"] != "session.resume" || ev["last_received_server_event_seq"] != float64(1) {
				t.Errorf("resume %#v", ev)
				return
			}
			_ = conn.WriteJSON(map[string]any{"type": "session.resumed", "resume_token": "rt2"})
			_ = conn.WriteJSON(map[string]any{
				"type": "response.audio.delta", "audio": base64.StdEncoding.EncodeToString(first),
				"sample_rate_hz": 24000, "server_event_seq": 1,
			})
			_ = conn.WriteJSON(map[string]any{
				"type": "response.audio.delta", "audio": base64.StdEncoding.EncodeToString(second),
				"sample_rate_hz": 24000, "server_event_seq": 2,
			})
			time.Sleep(300 * time.Millisecond)
			return
		}
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{
			"type":         "session.created",
			"resume_token": "rt1",
			"session": map[string]any{
				"id": "duplex-9",
				"capabilities": map[string]any{
					"supports_session_resume": true,
				},
			},
		})
		_ = conn.WriteJSON(map[string]any{
			"type": "response.output_audio.delta", "delta": base64.StdEncoding.EncodeToString(first),
			"sample_rate_hz": 24000, "server_event_seq": 1,
		})
		time.Sleep(50 * time.Millisecond)
		_ = conn.Close()
	}))
	defer srv.Close()

	p := NewOmni(srv.URL, WithHeartbeat(0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := p.Session(ctx, niro.RealtimeConfig{Model: "m", TurnDetection: "omit"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	var got [][]byte
	deadline := time.After(4 * time.Second)
	for len(got) < 2 {
		select {
		case <-deadline:
			t.Fatalf("audio %v", got)
		default:
		}
		if !sess.Recv().Next(ctx) {
			t.Fatal(sess.Err())
		}
		f := sess.Recv().Frame()
		if f.Kind == niro.KindAudio {
			got = append(got, append([]byte(nil), f.Data...))
		}
	}
	if string(got[0]) != string(first) || string(got[1]) != string(second) || len(got) != 2 {
		t.Fatalf("frames %v", got)
	}
}

func TestVLLMVideoTool(t *testing.T) {
	t.Parallel()
	jpeg := []byte{0xff, 0xd8, 1, 2}
	var mu sync.Mutex
	var events []map[string]any
	srv := serveWS(t, func(conn *websocket.Conn) {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		var update map[string]any
		_ = json.Unmarshal(msg, &update)
		mu.Lock()
		events = append(events, update)
		mu.Unlock()
		_ = conn.WriteJSON(map[string]any{
			"type":    "session.created",
			"session": map[string]any{"id": "s"},
		})
		for i := 0; i < 4; i++ {
			_, msg, err = conn.ReadMessage()
			if err != nil {
				t.Errorf("read %d: %v", i, err)
				return
			}
			var ev map[string]any
			_ = json.Unmarshal(msg, &ev)
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}
	})
	defer srv.Close()

	p := NewOmni(srv.URL, WithHeartbeat(0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := p.Session(ctx, niro.RealtimeConfig{
		Tools: []niro.Tool{{
			Name:        "capture",
			Description: "look at the camera",
			Kind:        niro.ToolKindVideo,
		}},
		ToolChoice: niro.ToolChoiceFunc("capture"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Send(ctx, niro.ToolResultFrame(&niro.ToolResult{
		CallID: "call_v",
		Parts: []niro.Part{
			{Kind: niro.KindImage, Data: jpeg, Mime: "image/jpeg"},
			{Kind: niro.KindVideo, URL: "https://cam.example/now.mp4", Mime: "video/mp4"},
		},
	})); err != nil {
		t.Fatal(err)
	}
	if err := sess.Send(ctx, niro.AudioFrame([]byte{9, 9}, "audio/pcm")); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n >= 5 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("got %d events", n)
		case <-time.After(20 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	session := events[0]["session"].(map[string]any)
	choice := session["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["name"] != "capture" {
		t.Fatalf("tool_choice %#v", choice)
	}
	tools := session["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "capture" {
		t.Fatalf("tools %#v", tools)
	}
	out := events[1]["item"].(map[string]any)
	if out["type"] != "function_call_output" || out["output"] != `{"frames":2}` {
		t.Fatalf("output %#v", out)
	}
	item := events[2]["item"].(map[string]any)
	content := item["content"].([]any)
	img := content[0].(map[string]any)
	if img["type"] != "input_image" || !strings.Contains(img["image_url"].(string), base64.StdEncoding.EncodeToString(jpeg)) {
		t.Fatalf("image %#v", img)
	}
	vid := content[1].(map[string]any)
	if vid["type"] != "input_video" || vid["video_url"] != "https://cam.example/now.mp4" {
		t.Fatalf("video %#v", vid)
	}
	if events[3]["type"] != "response.create" {
		t.Fatalf("create %#v", events[3])
	}
	frames := events[4]["video_frames"].([]any)
	if len(frames) != 1 || frames[0] != base64.StdEncoding.EncodeToString(jpeg) {
		t.Fatalf("video_frames %#v", frames)
	}
}

func TestCaptureBufferDropsOldest(t *testing.T) {
	b := NewCaptureBuffer(2)
	b.Push(niro.ImageFrame([]byte{1}, "image/jpeg"))
	b.Push(niro.VideoFrame([]byte{2}, "image/jpeg"))
	b.Push(niro.AudioFrame([]byte{9}, "audio/pcm"))
	b.Push(niro.ImageFrame([]byte{3}, "image/png"))
	got := b.Snapshot()
	if len(got) != 2 || got[0].Data[0] != 2 || got[1].Data[0] != 3 || got[1].Mime != "image/png" {
		t.Fatalf("%#v", got)
	}
	frame := CaptureResult(&niro.ToolCall{ID: "c"}, got)
	if frame.Result.CallID != "c" || len(frame.Result.Parts) != 2 {
		t.Fatalf("%#v", frame.Result)
	}
}

func serveWS(t *testing.T, fn func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		fn(conn)
	}))
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}
