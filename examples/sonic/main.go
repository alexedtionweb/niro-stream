// Command sonic demonstrates real-time speech-to-speech using Amazon Nova Sonic
// via the ryn.RealtimeProvider interface.
//
// The demo uses silence frames as synthetic audio input (no microphone required)
// and prints received text transcripts to stdout. Raw PCM audio output is
// discarded here; in production pipe it to your speaker/codec.
//
// Prerequisites:
//
//	AWS credentials configured (e.g. ~/.aws/credentials or env vars)
//	AWS_REGION set to a region where Nova Sonic is available (e.g. us-east-1)
//
// Run:
//
//	AWS_REGION=us-east-1 go run ./sonic
//
// Environment variables:
//
//	VOICE       synthesis voice (default: matthew)
//	CHUNK_MS    audio chunk size in milliseconds (default: 20)
//	LOG_LEVEL   set to "debug" for verbose output
//
// To stream real microphone audio, replace the silence loop in audioTurn with
// your PCM capture code. Chunks must be 16 kHz 16-bit mono PCM (ryn.AudioPCM16k).
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

	"github.com/aws/aws-sdk-go-v2/config"
	"ryn.dev/ryn"
	"ryn.dev/ryn/provider/bedrock"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		slog.Error("aws config", "err", err)
		os.Exit(1)
	}

	voice := os.Getenv("VOICE")
	if voice == "" {
		voice = "matthew"
	}

	chunkMs := 20
	if v := os.Getenv("CHUNK_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			chunkMs = n
		}
	}

	sonic := bedrock.NewSonic(awsCfg, bedrock.WithSonicVoice(voice))

	fmt.Printf("Provider : Amazon Nova Sonic\nVoice    : %s\n\n", voice)

	// ── Demo 1: text turn (no microphone required) ────────────────────────────
	fmt.Println("=== Text turn ===")
	if err := textTurn(ctx, sonic); err != nil {
		slog.Error("text turn failed", "err", err)
	}

	// ── Demo 2: simulated audio stream ───────────────────────────────────────
	fmt.Println("\n=== Simulated audio stream (silence) ===")
	if err := audioTurn(ctx, sonic, chunkMs); err != nil {
		slog.Error("audio turn failed", "err", err)
	}

	// ── Demo 3: tool calling ──────────────────────────────────────────────────
	fmt.Println("\n=== Tool calling ===")
	if err := toolTurn(ctx, sonic); err != nil {
		slog.Error("tool turn failed", "err", err)
	}
}

// ── Demo 1: text turn ─────────────────────────────────────────────────────────

func textTurn(ctx context.Context, p ryn.RealtimeProvider) error {
	sess, err := p.Session(ctx, ryn.RealtimeConfig{
		SystemPrompt: "You are a helpful voice assistant. Keep replies to one sentence.",
		Voice:        "",
	})
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer sess.Close()

	// Start the receive loop in a goroutine.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		printRecv(ctx, sess.Recv())
	}()

	// Send a text message then signal end-of-turn.
	if err := sess.Send(ctx, ryn.TextFrame("Hello! What time is it?")); err != nil {
		return fmt.Errorf("send text: %w", err)
	}
	if err := sess.Send(ctx, ryn.ControlFrame(ryn.SignalEOT)); err != nil {
		return fmt.Errorf("send eot: %w", err)
	}

	// Wait for the model to finish then close.
	wg.Wait()
	return sess.Err()
}

// ── Demo 2: audio turn (simulated with silence) ───────────────────────────────

// silencePCM returns a zeroed PCM16 frame of the given duration at 16 kHz mono.
func silencePCM(durationMs int) []byte {
	// 16 kHz × 16-bit (2 bytes/sample) × 1 channel
	samples := 16000 * durationMs / 1000
	return make([]byte, samples*2)
}

func audioTurn(ctx context.Context, p ryn.RealtimeProvider, chunkMs int) error {
	sess, err := p.Session(ctx, ryn.RealtimeConfig{
		SystemPrompt: "You are a helpful voice assistant. Keep replies to one sentence.",
		InputFormat:  ryn.AudioPCM16k, // 16 kHz PCM16 mono
		OutputFormat: ryn.AudioPCM24k, // 24 kHz PCM16 mono
		VAD:          &ryn.VADConfig{Threshold: 0.5, SilenceDurationMs: 300},
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

	// Stream ~500 ms of silence then end the turn.
	// In production: capture real PCM from your microphone here.
	chunk := silencePCM(chunkMs)
	nChunks := 500 / chunkMs
	for i := 0; i < nChunks; i++ {
		if err := sess.Send(ctx, ryn.AudioFrame(chunk, ryn.AudioPCM16k)); err != nil {
			return fmt.Errorf("send audio chunk %d: %w", i, err)
		}
		time.Sleep(time.Duration(chunkMs) * time.Millisecond)
	}
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
	})
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer sess.Close()

	// Recv loop handles tool calls and sends results back.
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
			case ryn.KindToolCall:
				fmt.Printf("\n[tool call] %s(%s)\n", f.Tool.Name, string(f.Tool.Args))
				// Execute the tool and send the result back.
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
			}
		}
		if err := stream.Err(); err != nil {
			slog.Error("recv stream", "err", err)
		}
	}()

	if err := sess.Send(ctx, ryn.TextFrame("What is the weather in Seattle?")); err != nil {
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
	// Stub: return synthetic data.
	return &ryn.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf(`{"city":%q,"temp_c":12,"condition":"cloudy"}`, args.City),
	}
}

// ── Recv loop helper ──────────────────────────────────────────────────────────

// printRecv reads from a Stream and prints text/audio/control frames until
// EOT or the stream closes.
func printRecv(ctx context.Context, stream *ryn.Stream) {
	for stream.Next(ctx) {
		f := stream.Frame()
		switch f.Kind {
		case ryn.KindText:
			fmt.Print(f.Text)
		case ryn.KindAudio:
			// In production: write f.Data (24 kHz PCM16 mono) to your speaker/codec.
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
				slog.Info("usage", "in", u.InputTokens, "out", u.OutputTokens)
			}
		}
	}
	if err := stream.Err(); err != nil {
		slog.Error("recv stream", "err", err)
	}
}
