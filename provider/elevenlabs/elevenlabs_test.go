package elevenlabs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/pool"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// collect drains a stream into frames and returns them with any error.
func collect(ctx context.Context, s *ryn.Stream) ([]ryn.Frame, error) {
	var out []ryn.Frame
	for s.Next(ctx) {
		f := s.Frame()
		// Copy Data to survive pool recycling
		if len(f.Data) > 0 {
			cp := make([]byte, len(f.Data))
			copy(cp, f.Data)
			f.Data = cp
		}
		out = append(out, f)
	}
	return out, s.Err()
}

// newTestProvider creates a Provider pointing at the given test server.
func newTestProvider(srv *httptest.Server, opts ...Option) *Provider {
	all := []Option{
		WithBaseURL(srv.URL),
		WithHTTPClient(srv.Client()),
	}
	all = append(all, opts...)
	return New("test-key", all...)
}

// ── New / Option tests ──────────────────────────────────────────────────────

func TestNewDefaults(t *testing.T) {
	p := New("key123")
	if p.apiKey != "key123" {
		t.Fatalf("apiKey = %q, want %q", p.apiKey, "key123")
	}
	if p.baseURL != defaultBaseURL {
		t.Fatalf("baseURL = %q, want %q", p.baseURL, defaultBaseURL)
	}
	if p.wsURL != defaultWSURL {
		t.Fatalf("wsURL = %q, want %q", p.wsURL, defaultWSURL)
	}
	if p.voice != defaultVoice {
		t.Fatalf("voice = %q, want %q", p.voice, defaultVoice)
	}
	if p.model != defaultModel {
		t.Fatalf("model = %q, want %q", p.model, defaultModel)
	}
	if p.speed != 1.0 {
		t.Fatalf("speed = %f, want 1.0", p.speed)
	}
}

func TestOptions(t *testing.T) {
	bp := pool.NewBytePool()
	c := &http.Client{}
	p := New("k",
		WithBaseURL("https://custom.api/v2/"),
		WithWSURL("wss://custom.ws/v1/stt"),
		WithHTTPClient(c),
		WithBytePool(bp),
		WithVoice("Bella"),
		WithModel("turbo_v2"),
		WithSTTModel("scribe_v2"),
		WithFormat("mp3_44100_128"),
		WithSpeed(1.5),
		WithLanguage("de"),
	)
	if p.baseURL != "https://custom.api/v2" {
		t.Fatalf("baseURL trailing slash not trimmed: %q", p.baseURL)
	}
	if p.wsURL != "wss://custom.ws/v1/stt" {
		t.Fatalf("wsURL = %q", p.wsURL)
	}
	if p.client != c {
		t.Fatal("client not set")
	}
	if p.bp != bp {
		t.Fatal("byte pool not set")
	}
	if p.voice != "Bella" || p.model != "turbo_v2" || p.sttModel != "scribe_v2" {
		t.Fatal("voice/model/sttModel not set")
	}
	if p.format != "mp3_44100_128" || p.speed != 1.5 || p.lang != "de" {
		t.Fatal("format/speed/lang not set")
	}
}

func TestInterfaceCompliance(t *testing.T) {
	var _ ryn.TTSProvider = (*Provider)(nil)
	var _ ryn.STTProvider = (*Provider)(nil)
}

// ── TTS tests ───────────────────────────────────────────────────────────────

func TestSynthesizeNilRequest(t *testing.T) {
	p := New("k")
	_, err := p.Synthesize(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "nil request") {
		t.Fatalf("expected nil request error, got %v", err)
	}
}

func TestSynthesizeEmptyText(t *testing.T) {
	p := New("k")
	_, err := p.Synthesize(context.Background(), &ryn.TTSRequest{Text: "  "})
	if err == nil || !strings.Contains(err.Error(), "empty text") {
		t.Fatalf("expected empty text error, got %v", err)
	}
}

func TestSynthesizeStreamsAudio(t *testing.T) {
	audio := bytes.Repeat([]byte{0xDE, 0xAD}, 4096) // 8KB

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validate request
		if r.Header.Get("xi-api-key") != "test-key" {
			t.Errorf("missing api key")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("bad content type: %s", r.Header.Get("Content-Type"))
		}
		if !strings.Contains(r.URL.Path, "/text-to-speech/") {
			t.Errorf("bad path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("output_format") == "" {
			t.Errorf("missing output_format query param")
		}

		// Verify JSON body has correct struct fields
		var body ttsBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("invalid json body: %v", err)
		}
		if body.Text != "hello world" {
			t.Errorf("text = %q, want %q", body.Text, "hello world")
		}
		if body.ModelID == "" {
			t.Errorf("model_id is empty")
		}

		w.Header().Set("Content-Type", "audio/ogg")
		w.WriteHeader(200)
		w.Write(audio)
	}))
	defer srv.Close()

	p := newTestProvider(srv)
	ctx := context.Background()
	stream, err := p.Synthesize(ctx, &ryn.TTSRequest{Text: "hello world"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	frames, err := collect(ctx, stream)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("no audio frames received")
	}

	var total int
	for _, f := range frames {
		if f.Kind != ryn.KindAudio {
			t.Errorf("expected KindAudio, got %v", f.Kind)
		}
		if f.Mime != ryn.AudioOGGOpus {
			t.Errorf("mime = %q, want %q", f.Mime, ryn.AudioOGGOpus)
		}
		total += len(f.Data)
	}
	if total != len(audio) {
		t.Errorf("received %d bytes, want %d", total, len(audio))
	}
}

func TestSynthesizeUsesRequestFields(t *testing.T) {
	var capturedPath, capturedQuery string
	var capturedBody ttsBody

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.WriteHeader(200)
		w.Write([]byte{0x01})
	}))
	defer srv.Close()

	p := newTestProvider(srv)
	ctx := context.Background()
	stream, err := p.Synthesize(ctx, &ryn.TTSRequest{
		Text:         "Hallo Welt",
		Voice:        "Bella",
		Model:        "turbo_v2",
		Language:     "de",
		OutputFormat: ryn.AudioMP3,
		Speed:        1.3,
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	collect(ctx, stream) // drain

	if !strings.Contains(capturedPath, "/Bella/") {
		t.Errorf("path should contain voice: %s", capturedPath)
	}
	if !strings.Contains(capturedQuery, "mp3_44100_128") {
		t.Errorf("query should contain mp3 format: %s", capturedQuery)
	}
	if capturedBody.ModelID != "turbo_v2" {
		t.Errorf("model_id = %q, want turbo_v2", capturedBody.ModelID)
	}
	if capturedBody.LanguageCode != "de" {
		t.Errorf("language_code = %q, want de", capturedBody.LanguageCode)
	}
	if capturedBody.VoiceSettings.Speed != 1.3 {
		t.Errorf("speed = %f, want 1.3", capturedBody.VoiceSettings.Speed)
	}
}

func TestSynthesizeAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid_api_key"}`))
	}))
	defer srv.Close()

	p := newTestProvider(srv)
	_, err := p.Synthesize(context.Background(), &ryn.TTSRequest{Text: "hello"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should contain status: %v", err)
	}
}

func TestSynthesizeContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Delay to let cancellation propagate
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	p := newTestProvider(srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := p.Synthesize(ctx, &ryn.TTSRequest{Text: "hello"})
	if err == nil {
		t.Fatal("expected error on canceled context")
	}
}

func TestSynthesizeRequestHook(t *testing.T) {
	var seenHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader = r.Header.Get("X-Custom")
		w.WriteHeader(200)
		w.Write([]byte{0x01})
	}))
	defer srv.Close()

	p := newTestProvider(srv, WithRequestHook(func(r *http.Request) {
		r.Header.Set("X-Custom", "from-hook")
	}))

	stream, err := p.Synthesize(context.Background(), &ryn.TTSRequest{Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	collect(context.Background(), stream)

	if seenHeader != "from-hook" {
		t.Fatalf("hook header = %q, want %q", seenHeader, "from-hook")
	}
}

func TestSynthesizeExtraHook(t *testing.T) {
	var seenHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader = r.Header.Get("X-Extra")
		w.WriteHeader(200)
		w.Write([]byte{0x01})
	}))
	defer srv.Close()

	p := newTestProvider(srv)
	stream, err := p.Synthesize(context.Background(), &ryn.TTSRequest{
		Text: "hi",
		Extra: RequestHook(func(r *http.Request) {
			r.Header.Set("X-Extra", "from-extra")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	collect(context.Background(), stream)

	if seenHeader != "from-extra" {
		t.Fatalf("extra hook header = %q, want %q", seenHeader, "from-extra")
	}
}

// ── STT batch tests ─────────────────────────────────────────────────────────

func TestTranscribeNilRequest(t *testing.T) {
	p := New("k")
	_, err := p.Transcribe(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "nil request") {
		t.Fatalf("expected nil request error, got %v", err)
	}
}

func TestTranscribeBatchEmptyAudio(t *testing.T) {
	p := New("k")
	_, err := p.Transcribe(context.Background(), &ryn.STTRequest{})
	if err == nil || !strings.Contains(err.Error(), "empty audio") {
		t.Fatalf("expected empty audio error, got %v", err)
	}
}

func TestTranscribeBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("xi-api-key") != "test-key" {
			t.Errorf("missing api key")
		}
		if !strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
			t.Errorf("bad content type: %s", r.Header.Get("Content-Type"))
		}
		if r.URL.Path != "/speech-to-text" {
			t.Errorf("bad path: %s", r.URL.Path)
		}

		// Verify multipart fields
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		if r.FormValue("model_id") != defaultSTTModel {
			t.Errorf("model_id = %q", r.FormValue("model_id"))
		}
		if r.FormValue("language_code") != "en" {
			t.Errorf("language_code = %q", r.FormValue("language_code"))
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("no file: %v", err)
		}
		if file != nil {
			data, _ := io.ReadAll(file)
			if len(data) != 100 {
				t.Errorf("file size = %d, want 100", len(data))
			}
			file.Close()
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":"Hello world"}`))
	}))
	defer srv.Close()

	p := newTestProvider(srv)
	ctx := context.Background()
	stream, err := p.Transcribe(ctx, &ryn.STTRequest{
		Audio:       make([]byte, 100),
		InputFormat: ryn.AudioWAV,
		Language:    "en",
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	frames, err := collect(ctx, stream)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if frames[0].Kind != ryn.KindText {
		t.Errorf("kind = %v, want KindText", frames[0].Kind)
	}
	if frames[0].Text != "Hello world" {
		t.Errorf("text = %q, want %q", frames[0].Text, "Hello world")
	}
}

func TestTranscribeBatchEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":""}`))
	}))
	defer srv.Close()

	p := newTestProvider(srv)
	ctx := context.Background()
	stream, err := p.Transcribe(ctx, &ryn.STTRequest{
		Audio:       []byte{1, 2, 3},
		InputFormat: ryn.AudioWAV,
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	frames, err := collect(ctx, stream)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(frames) != 0 {
		t.Fatalf("expected 0 frames for empty text, got %d", len(frames))
	}
}

func TestTranscribeBatchAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("server error"))
	}))
	defer srv.Close()

	p := newTestProvider(srv)
	_, err := p.Transcribe(context.Background(), &ryn.STTRequest{
		Audio:       []byte{1},
		InputFormat: ryn.AudioWAV,
	})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500 error, got %v", err)
	}
}

func TestTranscribeBatchRequestHook(t *testing.T) {
	var seenHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader = r.Header.Get("X-Trace")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":"ok"}`))
	}))
	defer srv.Close()

	p := newTestProvider(srv, WithRequestHook(func(r *http.Request) {
		r.Header.Set("X-Trace", "abc")
	}))

	stream, err := p.Transcribe(context.Background(), &ryn.STTRequest{
		Audio:       []byte{1},
		InputFormat: ryn.AudioWAV,
	})
	if err != nil {
		t.Fatal(err)
	}
	collect(context.Background(), stream)

	if seenHeader != "abc" {
		t.Fatalf("hook header = %q", seenHeader)
	}
}

// ── STT WebSocket streaming tests ───────────────────────────────────────────

// wsEcho is a test WebSocket server that echoes received audio as transcripts.
func wsEcho(t *testing.T, handler func(conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade error: %v", err)
			return
		}
		handler(conn)
	}))
}

func TestTranscribeStreamBasic(t *testing.T) {
	// WS server that reads 3 audio messages, sends final transcript, closes.
	var received atomic.Int32

	srv := wsEcho(t, func(conn *websocket.Conn) {
		defer conn.Close()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			// Validate it's valid JSON with audio_base_64
			var m map[string]string
			if json.Unmarshal(msg, &m) == nil {
				if _, ok := m["audio_base_64"]; ok {
					received.Add(1)
				}
			}
		}
		// Send final transcript
		resp, _ := json.Marshal(sttWSResponse{
			MessageType: "final_transcript",
			Text:        "hello world",
			IsFinal:     true,
		})
		conn.WriteMessage(websocket.TextMessage, resp)
		conn.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		)
	})
	defer srv.Close()

	// Create audio input stream with 3 frames
	audioStream, audioEmitter := ryn.NewStream(8)
	go func() {
		ctx := context.Background()
		for i := 0; i < 3; i++ {
			audioEmitter.Emit(ctx, ryn.AudioFrame([]byte{byte(i), byte(i)}, ryn.AudioPCM16k))
		}
		audioEmitter.Close()
	}()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	p := New("test-key", WithWSURL(wsURL))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := p.Transcribe(ctx, &ryn.STTRequest{
		AudioStream: audioStream,
		InputFormat: ryn.AudioPCM16k,
		Language:    "en",
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	frames, err := collect(ctx, stream)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if received.Load() != 3 {
		t.Errorf("server received %d audio messages, want 3", received.Load())
	}

	// We may get the transcript if the server sent it before close
	for _, f := range frames {
		if f.Kind != ryn.KindText {
			t.Errorf("kind = %v, want KindText", f.Kind)
		}
	}
}

func TestTranscribeStreamInterimResults(t *testing.T) {
	srv := wsEcho(t, func(conn *websocket.Conn) {
		defer conn.Close()

		// Read and discard audio
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()

		// Send interim then final
		time.Sleep(50 * time.Millisecond)
		msgs := []sttWSResponse{
			{MessageType: "partial", Text: "hel", IsFinal: false},
			{MessageType: "partial", Text: "hello", IsFinal: false},
			{MessageType: "final_transcript", Text: "hello world", IsFinal: true},
		}
		for _, m := range msgs {
			data, _ := json.Marshal(m)
			conn.WriteMessage(websocket.TextMessage, data)
			time.Sleep(20 * time.Millisecond)
		}

		conn.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		)
	})
	defer srv.Close()

	audioStream, audioEmitter := ryn.NewStream(8)
	go func() {
		ctx := context.Background()
		audioEmitter.Emit(ctx, ryn.AudioFrame([]byte{1}, ryn.AudioPCM16k))
		time.Sleep(100 * time.Millisecond)
		audioEmitter.Close()
	}()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	p := New("test-key", WithWSURL(wsURL))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := p.Transcribe(ctx, &ryn.STTRequest{
		AudioStream:    audioStream,
		InputFormat:    ryn.AudioPCM16k,
		InterimResults: true,
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	frames, err := collect(ctx, stream)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if len(frames) < 2 {
		t.Fatalf("expected at least 2 frames (interim + final), got %d", len(frames))
	}

	// Verify we got interim partials and a final
	var gotInterim, gotFinal bool
	for _, f := range frames {
		if f.Text == "hel" || f.Text == "hello" {
			gotInterim = true
		}
		if f.Text == "hello world" {
			gotFinal = true
		}
	}
	if !gotInterim {
		t.Error("expected interim results")
	}
	if !gotFinal {
		t.Error("expected final transcript")
	}
}

func TestTranscribeStreamNoInterim(t *testing.T) {
	srv := wsEcho(t, func(conn *websocket.Conn) {
		defer conn.Close()

		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()

		time.Sleep(50 * time.Millisecond)
		msgs := []sttWSResponse{
			{MessageType: "partial", Text: "hel", IsFinal: false},
			{MessageType: "final_transcript", Text: "hello", IsFinal: true},
		}
		for _, m := range msgs {
			data, _ := json.Marshal(m)
			conn.WriteMessage(websocket.TextMessage, data)
			time.Sleep(20 * time.Millisecond)
		}

		conn.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		)
	})
	defer srv.Close()

	audioStream, audioEmitter := ryn.NewStream(8)
	go func() {
		ctx := context.Background()
		audioEmitter.Emit(ctx, ryn.AudioFrame([]byte{1}, ryn.AudioPCM16k))
		time.Sleep(100 * time.Millisecond)
		audioEmitter.Close()
	}()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	p := New("test-key", WithWSURL(wsURL))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := p.Transcribe(ctx, &ryn.STTRequest{
		AudioStream:    audioStream,
		InputFormat:    ryn.AudioPCM16k,
		InterimResults: false, // Only finals
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	frames, err := collect(ctx, stream)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}

	// Should only have final, no partials
	for _, f := range frames {
		if f.Text == "hel" {
			t.Error("received interim result despite InterimResults=false")
		}
	}
	if len(frames) > 0 && frames[len(frames)-1].Text != "hello" {
		t.Errorf("last frame text = %q, want %q", frames[len(frames)-1].Text, "hello")
	}
}

func TestTranscribeStreamQueryParams(t *testing.T) {
	var capturedQuery string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		capturedQuery = r.URL.RawQuery
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conn.Close()
	}))
	defer srv.Close()

	audioStream, audioEmitter := ryn.NewStream(1)
	audioEmitter.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	p := New("test-key", WithWSURL(wsURL))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := p.Transcribe(ctx, &ryn.STTRequest{
		AudioStream:    audioStream,
		InputFormat:    ryn.AudioPCM16k,
		Model:          "scribe_v2",
		Language:       "fr",
		InterimResults: true,
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	collect(ctx, stream)

	if !strings.Contains(capturedQuery, "model_id=scribe_v2") {
		t.Errorf("missing model_id: %s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "language_code=fr") {
		t.Errorf("missing language_code: %s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "enable_partial_transcripts=true") {
		t.Errorf("missing enable_partial_transcripts: %s", capturedQuery)
	}
}

func TestTranscribeStreamAudioEncoding(t *testing.T) {
	// Verify that audio frames are sent as base64-encoded JSON.
	var receivedMsgs []string
	var mu sync.Mutex

	srv := wsEcho(t, func(conn *websocket.Conn) {
		defer conn.Close()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			mu.Lock()
			receivedMsgs = append(receivedMsgs, string(msg))
			mu.Unlock()
		}
	})
	defer srv.Close()

	audioData := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	audioStream, audioEmitter := ryn.NewStream(8)
	go func() {
		ctx := context.Background()
		audioEmitter.Emit(ctx, ryn.AudioFrame(audioData, ryn.AudioPCM16k))
		time.Sleep(50 * time.Millisecond)
		audioEmitter.Close()
	}()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	p := New("test-key", WithWSURL(wsURL))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := p.Transcribe(ctx, &ryn.STTRequest{
		AudioStream: audioStream,
		InputFormat: ryn.AudioPCM16k,
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	collect(ctx, stream)

	mu.Lock()
	defer mu.Unlock()

	if len(receivedMsgs) == 0 {
		t.Fatal("no messages received by server")
	}

	// Parse the message
	var m map[string]string
	if err := json.Unmarshal([]byte(receivedMsgs[0]), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	b64, ok := m["audio_base_64"]
	if !ok {
		t.Fatal("missing audio_base_64 field")
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("invalid base64: %v", err)
	}
	if !bytes.Equal(decoded, audioData) {
		t.Errorf("decoded = %v, want %v", decoded, audioData)
	}
}

// ── Helper tests ────────────────────────────────────────────────────────────

func TestMimeFromFormat(t *testing.T) {
	tests := []struct {
		format string
		want   string
	}{
		{"ogg_opus", ryn.AudioOGGOpus},
		{"mp3_44100_128", ryn.AudioMP3},
		{"pcm_16000", ryn.AudioPCM24k}, // pcm -> AudioPCM24k (default pcm)
		{"ulaw_8000", "audio/basic"},
		{"", ryn.AudioOGGOpus}, // default
	}
	for _, tt := range tests {
		got := mimeFromFormat(tt.format)
		if got != tt.want {
			t.Errorf("mimeFromFormat(%q) = %q, want %q", tt.format, got, tt.want)
		}
	}
}

func TestOutputFormatFromMIME(t *testing.T) {
	tests := []struct {
		mime string
		want string
	}{
		{ryn.AudioOGGOpus, "ogg_opus"},
		{ryn.AudioMP3, "mp3_44100_128"},
		{ryn.AudioAAC, "aac_44100"},
		{ryn.AudioPCM16k, "pcm_16000"},
		{ryn.AudioPCM24k, "pcm_24000"},
		{ryn.AudioPCM44k, "pcm_44100"},
		{ryn.AudioFLAC, "flac"},
		{ryn.AudioWAV, "wav"},
		{"", ""},
		{"unknown/type", ""},
	}
	for _, tt := range tests {
		got := outputFormatFromMIME(tt.mime)
		if got != tt.want {
			t.Errorf("outputFormatFromMIME(%q) = %q, want %q", tt.mime, got, tt.want)
		}
	}
}

func TestExtFromMIME(t *testing.T) {
	tests := []struct {
		mime string
		want string
	}{
		{ryn.AudioOGGOpus, ".ogg"},
		{ryn.AudioMP3, ".mp3"},
		{ryn.AudioWAV, ".wav"},
		{ryn.AudioFLAC, ".flac"},
		{"audio/pcm", ".raw"},
		{"something/else", ".bin"},
	}
	for _, tt := range tests {
		got := extFromMIME(tt.mime)
		if got != tt.want {
			t.Errorf("extFromMIME(%q) = %q, want %q", tt.mime, got, tt.want)
		}
	}
}

// ── ttsBody pool test ───────────────────────────────────────────────────────

func TestTTSBodyPoolReuse(t *testing.T) {
	b := acquireTTSBody("hello", "model1", "en", 1.5)
	if b.Text != "hello" || b.ModelID != "model1" || b.LanguageCode != "en" || b.VoiceSettings.Speed != 1.5 {
		t.Fatal("acquired body has wrong fields")
	}
	releaseTTSBody(b)

	// After release, fields should be zeroed
	if b.Text != "" || b.ModelID != "" {
		t.Fatal("released body should have zeroed fields")
	}
}

// ── Concurrent TTS test ─────────────────────────────────────────────────────

func TestSynthesizeConcurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write(bytes.Repeat([]byte{0xFF}, 128))
	}))
	defer srv.Close()

	p := newTestProvider(srv)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := p.Synthesize(ctx, &ryn.TTSRequest{Text: "concurrent"})
			if err != nil {
				t.Errorf("Synthesize: %v", err)
				return
			}
			frames, err := collect(ctx, stream)
			if err != nil {
				t.Errorf("stream error: %v", err)
				return
			}
			total := 0
			for _, f := range frames {
				total += len(f.Data)
			}
			if total != 128 {
				t.Errorf("got %d bytes, want 128", total)
			}
		}()
	}
	wg.Wait()
}

func TestTranscribeStreamWSAPIKey(t *testing.T) {
	var seenKey string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKey = r.Header.Get("Xi-Api-Key")
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conn.Close()
	}))
	defer srv.Close()

	audioStream, audioEmitter := ryn.NewStream(1)
	audioEmitter.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	p := New("secret-key-123", WithWSURL(wsURL))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := p.Transcribe(ctx, &ryn.STTRequest{
		AudioStream: audioStream,
		InputFormat: ryn.AudioPCM16k,
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	collect(ctx, stream)

	if seenKey != "secret-key-123" {
		t.Errorf("API key = %q, want %q", seenKey, "secret-key-123")
	}
}
