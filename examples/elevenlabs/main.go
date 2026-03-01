package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/provider/elevenlabs"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "tts":
		runTTS(os.Args[2:])
	case "stt":
		runSTT(os.Args[2:])
	case "stt-stream":
		runSTTStream(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "ElevenLabs demo (TTS + STT)")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Env:")
	fmt.Fprintln(os.Stderr, "  ELEVENLABS_API_KEY   required")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  go run ./elevenlabs tts -text \"Hello\" -out hello.mp3")
	fmt.Fprintln(os.Stderr, "  go run ./elevenlabs stt -in sample.wav")
	fmt.Fprintln(os.Stderr, "  go run ./elevenlabs stt-stream -in sample.pcm")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  tts   synthesize text to an audio file")
	fmt.Fprintln(os.Stderr, "  stt        transcribe an audio file to text (batch HTTP)")
	fmt.Fprintln(os.Stderr, "  stt-stream transcribe streamed audio frames to text (realtime WS)")
}

func runTTS(args []string) {
	fs := flag.NewFlagSet("tts", flag.ExitOnError)
	text := fs.String("text", "", "text to synthesize (if empty, read from stdin)")
	outPath := fs.String("out", "out.mp3", "output audio file path")
	voice := fs.String("voice", "", "ElevenLabs voice id/name (optional)")
	model := fs.String("model", "", "ElevenLabs TTS model id (optional)")
	lang := fs.String("lang", "", "language code hint, e.g. en (optional)")
	format := fs.String("format", "audio/mpeg", "output MIME type: audio/mpeg, audio/ogg;codecs=opus, audio/wav, ...")
	timeout := fs.Duration("timeout", 30*time.Second, "overall timeout")
	_ = fs.Parse(args)

	apiKey := strings.TrimSpace(os.Getenv("ELEVENLABS_API_KEY"))
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "ELEVENLABS_API_KEY is required")
		os.Exit(2)
	}

	inputText := strings.TrimSpace(*text)
	if inputText == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatal(err)
		}
		inputText = strings.TrimSpace(string(b))
	}
	if inputText == "" {
		fmt.Fprintln(os.Stderr, "empty text (provide -text or pipe stdin)")
		os.Exit(2)
	}

	p := elevenlabs.New(apiKey)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	stream, err := p.Synthesize(ctx, &niro.TTSRequest{
		Text:         inputText,
		Voice:        *voice,
		Model:        *model,
		Language:     *lang,
		OutputFormat: *format,
	})
	if err != nil {
		fatal(err)
	}

	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil && filepath.Dir(*outPath) != "." {
		fatal(err)
	}
	f, err := os.Create(*outPath)
	if err != nil {
		fatal(err)
	}
	defer f.Close()

	var n int64
	for stream.Next(ctx) {
		fr := stream.Frame()
		if fr.Kind != niro.KindAudio {
			continue
		}
		wrote, err := f.Write(fr.Data)
		if err != nil {
			fatal(err)
		}
		n += int64(wrote)
	}
	if err := stream.Err(); err != nil {
		fatal(err)
	}

	fmt.Fprintf(os.Stderr, "wrote %d bytes to %s\n", n, *outPath)
}

func runSTT(args []string) {
	fs := flag.NewFlagSet("stt", flag.ExitOnError)
	inPath := fs.String("in", "", "input audio file path")
	mime := fs.String("mime", "", "input MIME type (optional; inferred from extension if empty)")
	model := fs.String("model", "", "ElevenLabs STT model id (optional)")
	lang := fs.String("lang", "", "language code hint, e.g. en (optional)")
	timeout := fs.Duration("timeout", 30*time.Second, "overall timeout")
	_ = fs.Parse(args)

	apiKey := strings.TrimSpace(os.Getenv("ELEVENLABS_API_KEY"))
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "ELEVENLABS_API_KEY is required")
		os.Exit(2)
	}
	if strings.TrimSpace(*inPath) == "" {
		fmt.Fprintln(os.Stderr, "-in is required")
		os.Exit(2)
	}

	audio, err := os.ReadFile(*inPath)
	if err != nil {
		fatal(err)
	}

	inputMIME := strings.TrimSpace(*mime)
	if inputMIME == "" {
		inputMIME = inferMIMEFromPath(*inPath)
	}
	if inputMIME == "" {
		fmt.Fprintln(os.Stderr, "could not infer MIME type; pass -mime (e.g. audio/wav, audio/mpeg, audio/flac)")
		os.Exit(2)
	}

	p := elevenlabs.New(apiKey)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	stream, err := p.Transcribe(ctx, &niro.STTRequest{
		Audio:       audio,
		InputFormat: inputMIME,
		Model:       *model,
		Language:    *lang,
	})
	if err != nil {
		fatal(err)
	}

	var sb strings.Builder
	for stream.Next(ctx) {
		fr := stream.Frame()
		if fr.Kind == niro.KindText {
			sb.WriteString(fr.Text)
		}
	}
	if err := stream.Err(); err != nil {
		fatal(err)
	}

	fmt.Println(strings.TrimSpace(sb.String()))
}

func runSTTStream(args []string) {
	fs := flag.NewFlagSet("stt-stream", flag.ExitOnError)
	inPath := fs.String("in", "", "input raw PCM file path (recommended: 16kHz mono s16le)")
	model := fs.String("model", "", "ElevenLabs STT model id (optional)")
	lang := fs.String("lang", "", "language code hint, e.g. en (optional)")
	frameBytes := fs.Int("frame-bytes", 640, "bytes per audio frame to send (20ms @ 16kHz mono s16le is 640)")
	frameDelay := fs.Duration("frame-delay", 20*time.Millisecond, "delay between frames (simulate realtime)")
	timeout := fs.Duration("timeout", 30*time.Second, "overall timeout")
	_ = fs.Parse(args)

	apiKey := strings.TrimSpace(os.Getenv("ELEVENLABS_API_KEY"))
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "ELEVENLABS_API_KEY is required")
		os.Exit(2)
	}
	if strings.TrimSpace(*inPath) == "" {
		fmt.Fprintln(os.Stderr, "-in is required")
		os.Exit(2)
	}
	if *frameBytes <= 0 {
		fmt.Fprintln(os.Stderr, "-frame-bytes must be > 0")
		os.Exit(2)
	}

	audio, err := os.ReadFile(*inPath)
	if err != nil {
		fatal(err)
	}
	if len(audio) == 0 {
		fmt.Fprintln(os.Stderr, "empty input audio")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Create a stream of KindAudio frames and emit them with pacing.
	audioStream, emitter := niro.NewStream(32)
	go func() {
		defer emitter.Close()
		for off := 0; off < len(audio); off += *frameBytes {
			end := min(off+*frameBytes, len(audio))
			_ = emitter.Emit(ctx, niro.AudioFrame(audio[off:end], niro.AudioPCM16k))
			if *frameDelay > 0 {
				time.Sleep(*frameDelay)
			}
		}
	}()

	p := elevenlabs.New(apiKey)
	stream, err := p.Transcribe(ctx, &niro.STTRequest{
		AudioStream: audioStream,
		InputFormat: niro.AudioPCM16k,
		Model:       *model,
		Language:    *lang,
		// Set to true to see partials if the backend supports them.
		InterimResults: true,
	})
	if err != nil {
		fatal(err)
	}

	for stream.Next(ctx) {
		fr := stream.Frame()
		if fr.Kind == niro.KindText {
			fmt.Print(fr.Text)
		}
	}
	if err := stream.Err(); err != nil {
		fatal(err)
	}
	fmt.Println()
}

func inferMIMEFromPath(path string) string {
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
	default:
		return ""
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
