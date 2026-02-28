// Command realtime demonstrates real-time speech-to-speech using the
// OpenAI Realtime API via the ryn.RealtimeProvider interface.
//
// The demo uses silence frames as synthetic audio input (no microphone required)
// and prints received transcripts to stdout. Raw PCM audio output is discarded;
// in production pipe it to your speaker/codec.
//
// Prerequisites:
//
//	OPENAI_API_KEY environment variable must be set.
//
// Run:
//
//	OPENAI_API_KEY=sk-... go run ./realtime
//
// Environment variables:
//
//	OPENAI_API_KEY   required: your OpenAI API key
//	MODEL            model ID (default: gpt-4o-realtime-preview)
//	VOICE            synthesis voice (default: alloy)
//	CHUNK_MS         audio chunk size in milliseconds (default: 20)
//	LOG_LEVEL        set to "debug" for verbose output
//
// To stream real microphone audio, replace the silence loop in audioTurn with
// your PCM capture code. Chunks must be 24 kHz 16-bit mono PCM (ryn.AudioPCM24k).
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

	"ryn.dev/ryn"
	"ryn.dev/ryn/provider/realtime"
)

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY is required")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	model := os.Getenv("MODEL")
	if model == "" {
		model = "gpt-4o-realtime-preview"
	}
	voice := os.Getenv("VOICE")
	if voice == "" {
		voice = "alloy"
	}

	chunkMs := 20
	if v := os.Getenv("CHUNK_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			chunkMs = n
		}
	}

	p := realtime.New(apiKey,
		realtime.WithModel(model),
		realtime.WithVoice(voice),
	)

	fmt.Printf("Provider : OpenAI Realtime\nModel    : %s\nVoice    : %s\n\n", model, voice)

	// ── Demo 1: text turn (no microphone required) ────────────────────────────
	fmt.Println("=== Text turn ===")
	if err := textTurn(ctx, p); err != nil {
		slog.Error("text turn failed", "err", err)
	}

	// ── Demo 2: audio stream with server-side VAD ─────────────────────────────
	fmt.Println("\n=== Audio stream with server VAD (silence) ===")
	if err := audioTurn(ctx, p, chunkMs); err != nil {
		slog.Error("audio turn failed", "err", err)
	}

	// ── Demo 3: manual turn with tool call ────────────────────────────────────
	fmt.Println("\n=== Tool calling ===")
	if err := toolTurn(ctx, p); err != nil {
		slog.Error("tool turn failed", "err", err)
	}
}

// ── Demo 1: text turn ─────────────────────────────────────────────────────────

func textTurn(ctx context.Context, p ryn.RealtimeProvider) error {
	sess, err := p.Session(ctx, ryn.RealtimeConfig{
		SystemPrompt: "You are a helpful voice assistant. Keep replies to one sentence.",
	})
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

	if err := sess.Send(ctx, ryn.TextFrame("What is the capital of France?")); err != nil {
		return fmt.Errorf("send text: %w", err)
	}
	// SignalEOT commits the text buffer and requests a response.
	if err := sess.Send(ctx, ryn.ControlFrame(ryn.SignalEOT)); err != nil {
		return fmt.Errorf("send eot: %w", err)
	}

	wg.Wait()
	return sess.Err()
}

// ── Demo 2: audio turn with server VAD ───────────────────────────────────────

// silencePCM returns a zeroed PCM16 frame of the given duration at 24 kHz mono.
func silencePCM(durationMs int) []byte {
	// 24 kHz × 16-bit (2 bytes/sample) × 1 channel
	samples := 24000 * durationMs / 1000
	return make([]byte, samples*2)
}

func audioTurn(ctx context.Context, p ryn.RealtimeProvider, chunkMs int) error {
	sess, err := p.Session(ctx, ryn.RealtimeConfig{
		SystemPrompt: "You are a helpful voice assistant. Keep replies to one sentence.",
		InputFormat:  ryn.AudioPCM24k, // 24 kHz PCM16 mono
		OutputFormat: ryn.AudioPCM24k, // 24 kHz PCM16 mono
		VAD: &ryn.VADConfig{
			Threshold:         0.5,
			PrefixPaddingMs:   300,
			SilenceDurationMs: 200,
		},
	})
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
	chunk := silencePCM(chunkMs)
	nChunks := 1000 / chunkMs
	for i := 0; i < nChunks; i++ {
		if err := sess.Send(ctx, ryn.AudioFrame(chunk, ryn.AudioPCM24k)); err != nil {
			return fmt.Errorf("send audio chunk %d: %w", i, err)
		}
		time.Sleep(time.Duration(chunkMs) * time.Millisecond)
	}
	// Commit the buffer and request a response (VAD=none behaviour).
	if err := sess.Send(ctx, ryn.ControlFrame(ryn.SignalEOT)); err != nil {
		return fmt.Errorf("send eot: %w", err)
	}

	wg.Wait()
	return sess.Err()
}

// ── Demo 3: tool calling ──────────────────────────────────────────────────────

func toolTurn(ctx context.Context, p ryn.RealtimeProvider) error {
	weatherTool := ryn.Tool{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}

	sess, err := p.Session(ctx, ryn.RealtimeConfig{
		SystemPrompt: "You are a helpful voice assistant.",
		Tools:        []ryn.Tool{weatherTool},
		// Disable VAD so we control turn boundaries explicitly.
	})
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
			case ryn.KindText:
				fmt.Print(f.Text)
			case ryn.KindAudio:
				// discard audio output in this demo
				slog.Debug("audio frame", "bytes", len(f.Data))
			case ryn.KindToolCall:
				fmt.Printf("\n[tool call] %s(%s)\n", f.Tool.Name, string(f.Tool.Args))
				result := executeWeather(f.Tool)
				if sendErr := sess.Send(ctx, ryn.Frame{
					Kind:   ryn.KindToolResult,
					Result: result,
				}); sendErr != nil {
					slog.Error("send tool result", "err", sendErr)
				}
			case ryn.KindControl:
				if f.Signal == ryn.SignalEOT {
					fmt.Println()
					return
				}
				if f.Signal == ryn.SignalFlush {
					fmt.Println("\n[barge-in detected]")
				}
			case ryn.KindUsage:
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

	if err := sess.Send(ctx, ryn.TextFrame("What's the weather like in Tokyo?")); err != nil {
		return fmt.Errorf("send text: %w", err)
	}
	if err := sess.Send(ctx, ryn.ControlFrame(ryn.SignalEOT)); err != nil {
		return fmt.Errorf("send eot: %w", err)
	}

	wg.Wait()
	return sess.Err()
}

func executeWeather(call *ryn.ToolCall) *ryn.ToolResult {
	var args struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return &ryn.ToolResult{CallID: call.ID, IsError: true, Content: err.Error()}
	}
	return &ryn.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf(`{"city":%q,"temp_c":18,"condition":"sunny"}`, args.City),
	}
}

// ── Recv loop helper ──────────────────────────────────────────────────────────

func printRecv(ctx context.Context, stream *ryn.Stream) {
	for stream.Next(ctx) {
		f := stream.Frame()
		switch f.Kind {
		case ryn.KindText:
			fmt.Print(f.Text)
		case ryn.KindAudio:
			// In production: write f.Data (24 kHz PCM16 mono) to your speaker.
			slog.Debug("audio frame", "bytes", len(f.Data))
		case ryn.KindControl:
			switch f.Signal {
			case ryn.SignalEOT:
				fmt.Println()
				return
			case ryn.SignalFlush:
				fmt.Println("\n[barge-in detected]")
			}
		case ryn.KindUsage:
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
