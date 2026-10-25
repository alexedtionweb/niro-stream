// Command omni streams a vLLM-Omni chat completion.
//
//	VLLM_BASE_URL=http://127.0.0.1:8091 MODEL=<served-model> go run ./omni
//
// Environment variables:
//
//	VLLM_BASE_URL  required: server root
//	MODEL          required: model id the server is serving
//	PROMPT         user text; a short line is used when unset
//	IMAGE          optional image file or http(s) URL
//	VIDEO          optional video file or http(s) URL
//	AUDIO          optional audio file or http(s) URL
//	MODALITIES     comma-separated output modalities; text when unset
//	SPEAKER        optional talker name; omitted when empty
//	AUDIO_OUT      file that receives streamed audio bytes
//	VLLM_API_KEY   optional bearer token
package main

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/provider/vllmomni"
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

	prompt := os.Getenv("PROMPT")
	if prompt == "" {
		prompt = "Reply in one sentence."
	}
	parts := []niro.Part{niro.TextPart(prompt)}
	for _, item := range []struct {
		env  string
		kind niro.Kind
	}{
		{"IMAGE", niro.KindImage},
		{"VIDEO", niro.KindVideo},
		{"AUDIO", niro.KindAudio},
	} {
		ref := os.Getenv(item.env)
		if ref == "" {
			continue
		}
		part, err := mediaPart(item.kind, ref)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", item.env, err)
			os.Exit(1)
		}
		parts = append(parts, part)
	}

	opts := []vllmomni.Option{vllmomni.WithModel(model)}
	var modalities []string
	if raw := os.Getenv("MODALITIES"); raw != "" {
		for _, m := range strings.Split(raw, ",") {
			if m = strings.TrimSpace(m); m != "" {
				modalities = append(modalities, m)
			}
		}
		opts = append(opts, vllmomni.WithModalities(modalities...))
	}
	speaker := os.Getenv("SPEAKER")
	if speaker != "" {
		opts = append(opts, vllmomni.WithSpeaker(speaker))
	}
	llm := vllmomni.New(baseURL, os.Getenv("VLLM_API_KEY"), opts...)

	req := &niro.Request{
		Model:    model,
		Messages: []niro.Message{niro.Multi(niro.RoleUser, parts...)},
	}
	if speaker != "" || len(modalities) > 0 {
		req.Extra = vllmomni.Params{Speaker: speaker, Modalities: modalities}
	}

	stream, err := llm.Generate(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate: %v\n", err)
		os.Exit(1)
	}

	var audio io.WriteCloser
	if path := os.Getenv("AUDIO_OUT"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audio out: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		audio = f
	}

	var audioMIME string
	var audioBytes int
	for stream.Next(ctx) {
		f := stream.Frame()
		switch f.Kind {
		case niro.KindText:
			fmt.Print(f.Text)
		case niro.KindAudio:
			audioBytes += len(f.Data)
			if f.Mime != "" {
				audioMIME = f.Mime
			}
			if audio != nil && len(f.Data) > 0 {
				if _, err := audio.Write(f.Data); err != nil {
					fmt.Fprintf(os.Stderr, "\nwrite audio: %v\n", err)
					os.Exit(1)
				}
			}
		}
	}
	fmt.Println()
	if err := stream.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "stream: %v\n", err)
		os.Exit(1)
	}
	if audioBytes > 0 {
		fmt.Fprintf(os.Stderr, "audio %d bytes %s\n", audioBytes, audioMIME)
	}
	u := stream.Usage()
	fmt.Fprintf(os.Stderr, "tokens in %d out %d\n", u.InputTokens, u.OutputTokens)
}

func mediaPart(kind niro.Kind, ref string) (niro.Part, error) {
	mimeType := mime.TypeByExtension(filepath.Ext(ref))
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return niro.Part{Kind: kind, URL: ref, Mime: mimeType}, nil
	}
	data, err := os.ReadFile(ref)
	if err != nil {
		return niro.Part{}, err
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	return niro.Part{Kind: kind, Data: data, Mime: mimeType}, nil
}
