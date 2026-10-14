// Command realtime demonstrates speech-to-speech against a vLLM-Omni server
// (ws://<host>/v1/realtime) via niro.RealtimeProvider.
//
// The demo uses silence frames as synthetic audio input (no microphone required)
// and prints received transcripts to stdout. Raw PCM audio output is discarded;
// in production pipe it to your speaker/codec.
//
// Run:
//
//	VLLM_BASE_URL=http://127.0.0.1:8091 MODEL=<served-model> go run ./realtime
//
// Environment variables:
//
//	VLLM_BASE_URL  required: server root (http, https, ws, or wss)
//	MODEL          required: model id the server is serving
//	VOICE          optional synthesis voice; omitted when empty
//	SAMPLE_RATE    optional PCM rate for the audio demo; omitted when empty
//	VLLM_API_KEY   optional bearer token
//	CHUNK_MS       audio chunk size in milliseconds (default: 20)
//	LOG_LEVEL      set to "debug" for verbose output
//
// To stream real microphone audio, replace the silence loop in audioTurn with
// your PCM capture code and set SAMPLE_RATE to match it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/provider/realtime"
)

func main() {
	baseURL := os.Getenv("VLLM_BASE_URL")
	model := os.Getenv("MODEL")
	if baseURL == "" || model == "" {
		fmt.Fprintln(os.Stderr, "VLLM_BASE_URL and MODEL are required")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	voice := os.Getenv("VOICE")
	var sampleRate int
	if v := os.Getenv("SAMPLE_RATE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			fmt.Fprintln(os.Stderr, "SAMPLE_RATE must be a positive integer")
			os.Exit(1)
		}
		sampleRate = n
	}

	chunkMs := 20
	if v := os.Getenv("CHUNK_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			chunkMs = n
		}
	}

	opts := []realtime.Option{realtime.WithModel(model)}
	if voice != "" {
		opts = append(opts, realtime.WithVoice(voice))
	}
	if key := os.Getenv("VLLM_API_KEY"); key != "" {
		opts = append(opts, realtime.WithAPIKey(key))
	}
	p := realtime.NewOmni(baseURL, opts...)

	cfg := niro.RealtimeConfig{Model: model, Voice: voice}
	if sampleRate > 0 {
		cfg.InputFormat = pcmMIME(sampleRate)
		cfg.OutputFormat = cfg.InputFormat
	}

	fmt.Printf("Provider : vLLM-Omni\nEndpoint : %s\nModel    : %s\n", baseURL, model)
	if voice != "" {
		fmt.Printf("Voice    : %s\n", voice)
	}
	fmt.Println()

	// ── Demo 1: text turn (no microphone required) ────────────────────────────
	fmt.Println("=== Text turn ===")
	if err := textTurn(ctx, p, cfg); err != nil {
		slog.Error("text turn failed", "err", err)
	}

	// ── Demo 2: audio stream with server-side VAD ─────────────────────────────
	fmt.Println("\n=== Audio stream with server VAD (silence) ===")
	if sampleRate == 0 {
		fmt.Println("skipped: set SAMPLE_RATE to stream PCM")
	} else if err := audioTurn(ctx, p, cfg, chunkMs, sampleRate); err != nil {
		slog.Error("audio turn failed", "err", err)
	}

	// ── Demo 3: manual turn with tool call ────────────────────────────────────
	fmt.Println("\n=== Tool calling ===")
	if err := toolTurn(ctx, p, cfg); err != nil {
		slog.Error("tool turn failed", "err", err)
	}
}

func pcmMIME(rate int) string {
	return fmt.Sprintf("audio/pcm;rate=%d;bits=16;channels=1", rate)
}

// ── Demo 1: text turn ─────────────────────────────────────────────────────────

func textTurn(ctx context.Context, p niro.RealtimeProvider, cfg niro.RealtimeConfig) error {
	cfg.SystemPrompt = "You are a helpful voice assistant. Keep replies to one sentence."
	sess, err := p.Session(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer sess.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		printRecv(ctx, sess.Recv())
	}()

	if err := sess.Send(ctx, niro.TextFrame("What is the capital of France?")); err != nil {
		return fmt.Errorf("send text: %w", err)
	}
	// SignalEOT commits the text buffer and requests a response.
	if err := sess.Send(ctx, niro.ControlFrame(niro.SignalEOT)); err != nil {
		return fmt.Errorf("send eot: %w", err)
	}

	wg.Wait()
	return sess.Err()
}

// ── Demo 2: audio turn with server VAD ───────────────────────────────────────

func silencePCM(durationMs, rate int) []byte {
	samples := rate * durationMs / 1000
	return make([]byte, samples*2)
}

func audioTurn(ctx context.Context, p niro.RealtimeProvider, cfg niro.RealtimeConfig, chunkMs, rate int) error {
	cfg.SystemPrompt = "You are a helpful voice assistant. Keep replies to one sentence."
	cfg.VAD = &niro.VADConfig{
		Threshold:         0.5,
		PrefixPaddingMs:   300,
		SilenceDurationMs: 200,
	}
	sess, err := p.Session(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer sess.Close()

	// With server-side VAD enabled the server detects turn boundaries automatically.
	// EOT is still needed to flush a manual turn (VAD=none) or force generation.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		printRecv(ctx, sess.Recv())
	}()

	// Stream ~1 s of silence then manually commit. Real microphone audio would
	// trigger the VAD and cause the model to respond automatically.
	chunk := silencePCM(chunkMs, rate)
	nChunks := 1000 / chunkMs
	mime := cfg.InputFormat
	for i := 0; i < nChunks; i++ {
		if err := sess.Send(ctx, niro.AudioFrame(chunk, mime)); err != nil {
			return fmt.Errorf("send audio chunk %d: %w", i, err)
		}
		time.Sleep(time.Duration(chunkMs) * time.Millisecond)
	}
	// Commit the buffer and request a response (VAD=none behaviour).
	if err := sess.Send(ctx, niro.ControlFrame(niro.SignalEOT)); err != nil {
		return fmt.Errorf("send eot: %w", err)
	}

	wg.Wait()
	return sess.Err()
}

// ── Demo 3: tool calling ──────────────────────────────────────────────────────

func toolTurn(ctx context.Context, p niro.RealtimeProvider, cfg niro.RealtimeConfig) error {
	weatherTool := niro.Tool{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}
	// capture is a video tool: the model calls it when it wants to see
	// what is happening. Push camera frames into cam as they arrive.
	captureTool := niro.Tool{
		Name:        "capture",
		Description: "Look at the camera and describe what is happening right now.",
		Kind:        niro.ToolKindVideo,
	}
	cam := realtime.NewCaptureBuffer(4)

	cfg.SystemPrompt = "You are a helpful voice assistant."
	cfg.Tools = []niro.Tool{weatherTool, captureTool}
	cfg.VAD = nil
	cfg.TurnDetection = "manual"
	sess, err := p.Session(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer sess.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		stream := sess.Recv()
		for stream.Next(ctx) {
			f := stream.Frame()
			switch f.Kind {
			case niro.KindText:
				fmt.Print(f.Text)
			case niro.KindAudio:
				// discard audio output in this demo
				slog.Debug("audio frame", "bytes", len(f.Data))
			case niro.KindToolCall:
				fmt.Printf("\n[tool call] %s(%s)\n", f.Tool.Name, string(f.Tool.Args))
				var reply niro.Frame
				if f.Tool.Name == captureTool.Name {
					reply = realtime.CaptureResult(f.Tool, cam.Snapshot())
				} else {
					reply = niro.ToolResultFrame(executeWeather(f.Tool))
				}
				if sendErr := sess.Send(ctx, reply); sendErr != nil {
					slog.Error("send tool result", "err", sendErr)
				}
			case niro.KindControl:
				if f.Signal == niro.SignalEOT {
					fmt.Println()
					return
				}
				if f.Signal == niro.SignalFlush {
					fmt.Println("\n[barge-in detected]")
				}
			case niro.KindUsage:
				if u := f.Usage; u != nil {
					slog.Info("usage",
						"in", u.InputTokens,
						"out", u.OutputTokens,
						"in_audio", u.Detail["input_audio_tokens"],
						"out_audio", u.Detail["output_audio_tokens"],
					)
				}
			}
		}
		if err := stream.Err(); err != nil {
			slog.Error("recv stream", "err", err)
		}
	}()

	if err := sess.Send(ctx, niro.TextFrame("What's the weather like in Tokyo?")); err != nil {
		return fmt.Errorf("send text: %w", err)
	}
	if err := sess.Send(ctx, niro.ControlFrame(niro.SignalEOT)); err != nil {
		return fmt.Errorf("send eot: %w", err)
	}

	wg.Wait()
	return sess.Err()
}

func executeWeather(call *niro.ToolCall) *niro.ToolResult {
	var args struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return &niro.ToolResult{CallID: call.ID, IsError: true, Content: err.Error()}
	}
	return &niro.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf(`{"city":%q,"temp_c":18,"condition":"sunny"}`, args.City),
	}
}

// ── Recv loop helper ──────────────────────────────────────────────────────────

func printRecv(ctx context.Context, stream *niro.Stream) {
	for stream.Next(ctx) {
		f := stream.Frame()
		switch f.Kind {
		case niro.KindText:
			fmt.Print(f.Text)
		case niro.KindAudio:
			// In production: write f.Data to your speaker. The MIME is f.Mime.
			slog.Debug("audio frame", "bytes", len(f.Data))
		case niro.KindControl:
			switch f.Signal {
			case niro.SignalEOT:
				fmt.Println()
				return
			case niro.SignalFlush:
				fmt.Println("\n[barge-in detected]")
			}
		case niro.KindUsage:
			if u := f.Usage; u != nil {
				slog.Info("usage",
					"in", u.InputTokens,
					"out", u.OutputTokens,
					"in_audio", u.Detail["input_audio_tokens"],
					"out_audio", u.Detail["output_audio_tokens"],
				)
			}
		}
	}
	if err := stream.Err(); err != nil {
		slog.Error("recv stream", "err", err)
	}
}
