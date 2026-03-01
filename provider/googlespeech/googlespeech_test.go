package googlespeech

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"cloud.google.com/go/speech/apiv1/speechpb"
	"cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
	"github.com/googleapis/gax-go/v2"

	"github.com/alexedtionweb/niro-stream"
)

type fakeClient struct {
	synthesizeResp *texttospeechpb.SynthesizeSpeechResponse
	synthesizeErr  error
	stream         streamingClient
	streamErr      error
	recognizeResp  *speechpb.RecognizeResponse
	recognizeErr   error
	sttStream      sttStreamingClient
	sttStreamErr   error
}

func (f *fakeClient) SynthesizeSpeech(ctx context.Context, req *texttospeechpb.SynthesizeSpeechRequest, opts ...gax.CallOption) (*texttospeechpb.SynthesizeSpeechResponse, error) {
	if f.synthesizeErr != nil {
		return nil, f.synthesizeErr
	}
	return f.synthesizeResp, nil
}

func (f *fakeClient) StreamingSynthesize(ctx context.Context, opts ...gax.CallOption) (streamingClient, error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return f.stream, nil
}

func (f *fakeClient) Close() error { return nil }

func (f *fakeClient) Recognize(ctx context.Context, req *speechpb.RecognizeRequest, opts ...gax.CallOption) (*speechpb.RecognizeResponse, error) {
	if f.recognizeErr != nil {
		return nil, f.recognizeErr
	}
	return f.recognizeResp, nil
}

func (f *fakeClient) StreamingRecognize(ctx context.Context, opts ...gax.CallOption) (sttStreamingClient, error) {
	if f.sttStreamErr != nil {
		return nil, f.sttStreamErr
	}
	return f.sttStream, nil
}

type fakeStreamClient struct {
	sendErr  error
	recvErr  error
	chunks   [][]byte
	recvCall int
}

func (f *fakeStreamClient) Send(*texttospeechpb.StreamingSynthesizeRequest) error {
	return f.sendErr
}

func (f *fakeStreamClient) Recv() (*texttospeechpb.StreamingSynthesizeResponse, error) {
	if f.recvErr != nil {
		return nil, f.recvErr
	}
	if f.recvCall >= len(f.chunks) {
		return nil, io.EOF
	}
	chunk := f.chunks[f.recvCall]
	f.recvCall++
	return &texttospeechpb.StreamingSynthesizeResponse{AudioContent: chunk}, nil
}

func (f *fakeStreamClient) CloseSend() error { return nil }

type fakeSTTStreamClient struct {
	sendErr  error
	recvErr  error
	resps    []*speechpb.StreamingRecognizeResponse
	recvCall int
}

func (f *fakeSTTStreamClient) Send(*speechpb.StreamingRecognizeRequest) error {
	return f.sendErr
}

func (f *fakeSTTStreamClient) Recv() (*speechpb.StreamingRecognizeResponse, error) {
	if f.recvErr != nil {
		return nil, f.recvErr
	}
	if f.recvCall >= len(f.resps) {
		return nil, io.EOF
	}
	resp := f.resps[f.recvCall]
	f.recvCall++
	return resp, nil
}

func (f *fakeSTTStreamClient) CloseSend() error { return nil }

func TestSynthesize_BatchChunks(t *testing.T) {
	client := &fakeClient{
		synthesizeResp: &texttospeechpb.SynthesizeSpeechResponse{
			AudioContent: []byte("abcdefghijkl"),
		},
	}
	p, err := New(context.Background(),
		WithClient(client),
		WithChunkSize(4),
		WithEncoding(texttospeechpb.AudioEncoding_MP3),
		WithRequestTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stream, err := p.Synthesize(context.Background(), &niro.TTSRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	var got [][]byte
	for stream.Next(context.Background()) {
		frame := stream.Frame()
		if frame.Kind != niro.KindAudio {
			t.Fatalf("frame kind = %v, want KindAudio", frame.Kind)
		}
		got = append(got, append([]byte(nil), frame.Data...))
		if frame.Mime != niro.AudioMP3 {
			t.Fatalf("mime = %q, want %q", frame.Mime, niro.AudioMP3)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("chunks = %d, want 3", len(got))
	}
	if string(got[0]) != "abcd" || string(got[1]) != "efgh" || string(got[2]) != "ijkl" {
		t.Fatalf("unexpected chunks: %q %q %q", got[0], got[1], got[2])
	}
}

func TestSynthesize_StreamingPath(t *testing.T) {
	client := &fakeClient{
		stream: &fakeStreamClient{
			chunks: [][]byte{[]byte("a"), []byte("bc")},
		},
		synthesizeResp: &texttospeechpb.SynthesizeSpeechResponse{
			AudioContent: []byte("fallback"),
		},
	}
	p, err := New(context.Background(),
		WithClient(client),
		WithStreaming(true),
		WithVoice("en-US-Chirp3-HD-Achernar"),
		WithEncoding(texttospeechpb.AudioEncoding_OGG_OPUS),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stream, err := p.Synthesize(context.Background(), &niro.TTSRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	audio, err := niro.Collect(context.Background(), stream)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(audio) != 2 {
		t.Fatalf("frames = %d, want 2", len(audio))
	}
	if string(audio[0].Data) != "a" || string(audio[1].Data) != "bc" {
		t.Fatalf("unexpected stream chunks")
	}
}

func TestSynthesize_StreamingFallbackToBatch(t *testing.T) {
	client := &fakeClient{
		stream: &fakeStreamClient{sendErr: errors.New("stream fail")},
		synthesizeResp: &texttospeechpb.SynthesizeSpeechResponse{
			AudioContent: []byte("batched"),
		},
	}
	p, err := New(context.Background(),
		WithClient(client),
		WithStreaming(true),
		WithVoice("not-streaming-voice"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stream, err := p.Synthesize(context.Background(), &niro.TTSRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	frames, err := niro.Collect(context.Background(), stream)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if string(frames[0].Data) != "batched" {
		t.Fatalf("audio = %q, want batched", frames[0].Data)
	}
}

func TestSynthesize_ExtraOverrides(t *testing.T) {
	client := &fakeClient{
		synthesizeResp: &texttospeechpb.SynthesizeSpeechResponse{
			AudioContent: []byte("x"),
		},
	}
	p, err := New(context.Background(),
		WithClient(client),
		WithLanguage("en-US"),
		WithVoice("voice-a"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stream, err := p.Synthesize(context.Background(), &niro.TTSRequest{
		Text: "hello",
		Extra: map[string]any{
			"tts": map[string]any{
				"voice":       "voice-b",
				"language":    "pt-BR",
				"encoding":    "mp3",
				"sample_rate": "24000",
				"speed":       "1.1",
			},
		},
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	for stream.Next(context.Background()) {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
}

func TestSynthesize_ValidationErrors(t *testing.T) {
	p, err := New(context.Background(), WithClient(&fakeClient{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Synthesize(context.Background(), nil); err == nil {
		t.Fatal("expected nil request error")
	}
	if _, err := p.Synthesize(context.Background(), &niro.TTSRequest{Text: "  "}); err == nil {
		t.Fatal("expected empty text error")
	}
}

func TestTranscribe_Batch(t *testing.T) {
	client := &fakeClient{
		recognizeResp: &speechpb.RecognizeResponse{
			Results: []*speechpb.SpeechRecognitionResult{
				{
					Alternatives: []*speechpb.SpeechRecognitionAlternative{
						{Transcript: "hello world"},
					},
				},
			},
		},
	}
	p, err := New(context.Background(), WithClient(client), WithSTTClient(client))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stream, err := p.Transcribe(context.Background(), &niro.STTRequest{
		Audio:       []byte{1, 2, 3},
		InputFormat: niro.AudioPCM16k,
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	text, err := niro.CollectText(context.Background(), stream)
	if err != nil {
		t.Fatalf("CollectText: %v", err)
	}
	if text != "hello world" {
		t.Fatalf("text = %q, want hello world", text)
	}
}

func TestTranscribe_Streaming(t *testing.T) {
	sttStream := &fakeSTTStreamClient{
		resps: []*speechpb.StreamingRecognizeResponse{
			{
				Results: []*speechpb.StreamingRecognitionResult{
					{
						Alternatives: []*speechpb.SpeechRecognitionAlternative{
							{Transcript: "hel"},
						},
					},
				},
			},
			{
				Results: []*speechpb.StreamingRecognitionResult{
					{
						Alternatives: []*speechpb.SpeechRecognitionAlternative{
							{Transcript: "hello"},
						},
						IsFinal: true,
					},
				},
			},
		},
	}
	client := &fakeClient{sttStream: sttStream}
	p, err := New(context.Background(), WithClient(client), WithSTTClient(client))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	audioStream := niro.StreamFromSlice([]niro.Frame{
		niro.AudioFrame([]byte{1, 2}, niro.AudioPCM16k),
		niro.AudioFrame([]byte{3, 4}, niro.AudioPCM16k),
	})
	stream, err := p.Transcribe(context.Background(), &niro.STTRequest{
		AudioStream:    audioStream,
		InputFormat:    niro.AudioPCM16k,
		InterimResults: true,
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	text, err := niro.CollectText(context.Background(), stream)
	if err != nil {
		t.Fatalf("CollectText: %v", err)
	}
	if text != "helhello" {
		t.Fatalf("text = %q, want helhello", text)
	}
}

func TestTranscribe_ValidationErrors(t *testing.T) {
	p, err := New(context.Background(), WithClient(&fakeClient{}), WithSTTClient(&fakeClient{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Transcribe(context.Background(), nil); err == nil {
		t.Fatal("expected nil request error")
	}
	if _, err := p.Transcribe(context.Background(), &niro.STTRequest{}); err == nil {
		t.Fatal("expected empty audio error")
	}
}
