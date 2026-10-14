// Command ollama is a working example of using niro-stream against a local
// Ollama server.
//
// Ollama exposes an OpenAI-compatible chat-completions endpoint at
// http://localhost:11434/v1/chat/completions, so the existing
// niro-stream/provider/compat provider works out of the box.
//
// Subcommands:
//
//	chat   stream a single text prompt (default)
//	image  stream a multimodal prompt (text + image) for vision models
//	voice  transcribe an audio file with ElevenLabs STT, then stream
//	       the transcript through an Ollama model
//
// Examples:
//
//	# Make sure Ollama is running and the model is pulled:
//	#   ollama serve
//	#   ollama pull gemma3n:e2b
//
//	go run ./ollama chat -prompt "Hello!"
//	go run ./ollama chat -model gemma4:e2b -prompt "Write a haiku about Go"
//
//	go run ./ollama image -model qwen2.5vl -image ./cat.jpg \
//	    -prompt "Describe this picture in one sentence."
//
//	# Audio path: ElevenLabs STT -> Ollama LLM
//	# Ollama core does not yet accept raw audio in /api/chat, so we
//	# transcribe first and feed the text into the model.
//	ELEVENLABS_API_KEY=... go run ./ollama voice -in ./hello.wav \
//	    -model gemma4:e2b
//
// Override the endpoint with OLLAMA_BASE_URL (defaults to
// http://localhost:11434/v1).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/provider/compat"
	"github.com/alexedtionweb/niro-stream/provider/elevenlabs"
)

const defaultOllamaBaseURL = "http://localhost:11434/v1"

func main() {
	cmd := "chat"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "chat":
		runChat(args)
	case "image":
		runImage(args)
	case "voice", "audio":
		runVoice(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Ollama + niro-stream example")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  go run ./ollama chat  [-model …] [-prompt …]")
	fmt.Fprintln(os.Stderr, "  go run ./ollama image [-model …] -image PATH [-prompt …]")
	fmt.Fprintln(os.Stderr, "  go run ./ollama voice [-model …] -in AUDIO_FILE")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Env:")
	fmt.Fprintln(os.Stderr, "  OLLAMA_BASE_URL    OpenAI-compatible endpoint  (default http://localhost:11434/v1)")
	fmt.Fprintln(os.Stderr, "  ELEVENLABS_API_KEY required for `voice`")
}

// ── chat: text streaming via Ollama (OpenAI-compat) ──────────────────────────

func runChat(args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	model := fs.String("model", "gemma4:e2b", "Ollama model tag")
	prompt := fs.String("prompt", "Hello!", "user prompt")
	system := fs.String("system", "", "optional system prompt")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")
	_ = fs.Parse(args)

	llm := newOllamaProvider(*model)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	stream, err := llm.Generate(ctx, &niro.Request{
		Model:        *model,
		SystemPrompt: *system,
		Messages:     []niro.Message{niro.UserText(*prompt)},
	})
	if err != nil {
		fatal(err)
	}

	streamText(ctx, stream)
}

// ── image: multimodal (text + image) via Ollama (OpenAI-compat) ──────────────

func runImage(args []string) {
	fs := flag.NewFlagSet("image", flag.ExitOnError)
	model := fs.String("model", "qwen2.5vl", "Ollama vision model tag")
	prompt := fs.String("prompt", "Describe this image.", "user prompt")
	imagePath := fs.String("image", "", "path to an image file (PNG, JPEG, ...)")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")
	_ = fs.Parse(args)

	if strings.TrimSpace(*imagePath) == "" {
		fatal(errors.New("-image is required"))
	}
	data, err := os.ReadFile(*imagePath)
	if err != nil {
		fatal(err)
	}
	imgMime := mimeFromExt(*imagePath)
	if imgMime == "" {
		imgMime = http.DetectContentType(data)
	}

	llm := newOllamaProvider(*model)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	msg := niro.Multi(niro.RoleUser,
		niro.TextPart(*prompt),
		niro.ImagePart(data, imgMime),
	)

	stream, err := llm.Generate(ctx, &niro.Request{
		Model:    *model,
		Messages: []niro.Message{msg},
	})
	if err != nil {
		fatal(err)
	}

	streamText(ctx, stream)
}

// ── voice: ElevenLabs STT → Ollama LLM ───────────────────────────────────────
//
// Ollama core does not yet accept raw audio in /api/chat. The pragmatic
// pattern is to transcribe first (any STT works — ElevenLabs is shown here
// because niro-stream ships a provider for it), then feed the transcript to
// the LLM.
//
//	mic / file → STT (ElevenLabs scribe_v1) → text → Ollama (gemma3n) → text
func runVoice(args []string) {
	fs := flag.NewFlagSet("voice", flag.ExitOnError)
	model := fs.String("model", "gemma4:e2b", "Ollama model tag")
	system := fs.String("system", "You are a helpful voice assistant. Reply concisely.", "optional system prompt")
	inPath := fs.String("in", "", "path to input audio file (wav, mp3, flac, ogg, m4a)")
	mimeType := fs.String("mime", "", "input audio MIME (auto-detected from extension if empty)")
	lang := fs.String("lang", "", "language hint for STT (e.g. en, pt, es)")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")
	_ = fs.Parse(args)

	apiKey := strings.TrimSpace(os.Getenv("ELEVENLABS_API_KEY"))
	if apiKey == "" {
		fatal(errors.New("ELEVENLABS_API_KEY is required for the voice command"))
	}
	if strings.TrimSpace(*inPath) == "" {
		fatal(errors.New("-in is required"))
	}

	audio, err := os.ReadFile(*inPath)
	if err != nil {
		fatal(err)
	}

	inMime := strings.TrimSpace(*mimeType)
	if inMime == "" {
		inMime = audioMimeFromExt(*inPath)
	}
	if inMime == "" {
		fatal(errors.New("could not infer audio MIME — pass -mime (e.g. audio/wav)"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	stt := elevenlabs.New(apiKey)
	sttStream, err := stt.Transcribe(ctx, &niro.STTRequest{
		Audio:       audio,
		InputFormat: inMime,
		Language:    *lang,
	})
	if err != nil {
		fatal(fmt.Errorf("stt: %w", err))
	}

	var transcript strings.Builder
	for sttStream.Next(ctx) {
		fr := sttStream.Frame()
		if fr.Kind == niro.KindText {
			transcript.WriteString(fr.Text)
		}
	}
	if err := sttStream.Err(); err != nil {
		fatal(fmt.Errorf("stt: %w", err))
	}

	prompt := strings.TrimSpace(transcript.String())
	if prompt == "" {
		fatal(errors.New("empty transcript — check audio file"))
	}

	fmt.Fprintf(os.Stderr, "── transcript ──\n%s\n── reply ──\n", prompt)

	llm := newOllamaProvider(*model)
	stream, err := llm.Generate(ctx, &niro.Request{
		Model:        *model,
		SystemPrompt: *system,
		Messages:     []niro.Message{niro.UserText(prompt)},
	})
	if err != nil {
		fatal(fmt.Errorf("ollama: %w", err))
	}

	streamText(ctx, stream)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func newOllamaProvider(model string) niro.Provider {
	baseURL := strings.TrimSpace(os.Getenv("OLLAMA_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultOllamaBaseURL
	}
	return compat.New(baseURL, "", compat.WithModel(model))
}

func streamText(ctx context.Context, stream *niro.Stream) {
	for stream.Next(ctx) {
		f := stream.Frame()
		if f.Kind == niro.KindText {
			fmt.Print(f.Text)
		}
	}
	fmt.Println()
	if err := stream.Err(); err != nil && !errors.Is(err, context.Canceled) {
		fatal(err)
	}
	if u := stream.Usage(); u.TotalTokens > 0 {
		fmt.Fprintf(os.Stderr, "(%d in · %d out · %d total)\n",
			u.InputTokens, u.OutputTokens, u.TotalTokens)
	}
}

func mimeFromExt(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	}
	return ""
}

func audioMimeFromExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".wav":
		return niro.AudioWAV
	case ".mp3":
		return niro.AudioMP3
	case ".flac":
		return niro.AudioFLAC
	case ".aac", ".m4a":
		return niro.AudioAAC
	case ".ogg", ".opus":
		return niro.AudioOGGOpus
	}
	return ""
}

func fatal(err error) {
	if errors.Is(err, io.EOF) {
		return
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
