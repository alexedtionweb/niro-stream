// Package audiomodel calls an HTTP VAD model and an HTTP turn-detection model.
//
// Neither model ships in this module. You run the server and install only
// this client:
//
//	go get github.com/alexedtionweb/niro-stream/provider/audiomodel
//
// # VAD model
//
// [Speech] POSTs each PCM16 chunk:
//
//	{"audio":"<standard base64>","sample_rate":16000}
//
// The server responds:
//
//	{"speech":0.82}
//
// speech is the probability that the chunk contains speech. On and Off are
// the probabilities that open and close a run. SpeechEnd also waits for Silence.
//
// # Turn model
//
// [Endpoint] POSTs:
//
//	{"text":"what you heard","silence_ms":240}
//
// The server responds:
//
//	{"end":0.91}
//
// end is the probability that the speaker has finished the turn.
package audiomodel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

// Speech implements [niro.VAD] by scoring each chunk with an HTTP model.
type Speech struct {
	// URL is the VAD model endpoint.
	URL string
	// Client is optional. Nil uses [http.DefaultClient].
	Client *http.Client
	// On opens speech. Default 0.5.
	On float32
	// Off is the probability below which silence starts counting. Default 0.35.
	Off float32
	// Silence must elapse below Off before SpeechEnd. Default 400ms.
	Silence time.Duration
	// Rate is the PCM sample rate sent to the model and used for timing.
	// Default 16000.
	Rate int

	speaking bool
	quiet    time.Duration
}

func (s *Speech) defaults() {
	if s.On <= 0 {
		s.On = 0.5
	}
	if s.Off <= 0 {
		s.Off = 0.35
	}
	if s.Silence <= 0 {
		s.Silence = 400 * time.Millisecond
	}
	if s.Rate <= 0 {
		s.Rate = 16000
	}
}

func (s *Speech) Reset() {
	s.speaking = false
	s.quiet = 0
}

func (s *Speech) Push(ctx context.Context, pcm []byte) ([]niro.SpeechMark, error) {
	s.defaults()
	n := len(pcm) / 2
	if n == 0 {
		return nil, nil
	}
	var out struct {
		Speech float32 `json:"speech"`
	}
	if err := post(ctx, s.Client, s.URL, map[string]any{
		"audio":       base64.StdEncoding.EncodeToString(pcm),
		"sample_rate": s.Rate,
	}, &out); err != nil {
		return nil, err
	}
	dur := time.Duration(n) * time.Second / time.Duration(s.Rate)
	if out.Speech >= s.On || (s.speaking && out.Speech >= s.Off) {
		s.quiet = 0
		if s.speaking {
			return nil, nil
		}
		s.speaking = true
		return []niro.SpeechMark{{Kind: niro.SpeechStart, At: dur, Probability: out.Speech}}, nil
	}
	if !s.speaking {
		return nil, nil
	}
	s.quiet += dur
	if s.quiet < s.Silence {
		return nil, nil
	}
	at := s.quiet
	s.speaking = false
	s.quiet = 0
	return []niro.SpeechMark{{Kind: niro.SpeechEnd, At: at, Probability: out.Speech}}, nil
}

// Endpoint implements [niro.TurnDetector] by calling an HTTP model.
type Endpoint struct {
	URL    string
	Client *http.Client
}

func (e Endpoint) Predict(ctx context.Context, in niro.TurnInput) (niro.TurnScore, error) {
	var out struct {
		End float32 `json:"end"`
	}
	err := post(ctx, e.Client, e.URL, map[string]any{
		"text":       in.Text,
		"silence_ms": in.Silence.Milliseconds(),
	}, &out)
	if err != nil {
		return niro.TurnScore{}, err
	}
	return niro.TurnScore{End: out.End}, nil
}

func post(ctx context.Context, client *http.Client, url string, body any, dst any) error {
	if client == nil {
		client = http.DefaultClient
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	slurp, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return niro.NewErrorf(niro.ConvertHTTPStatusToCode(resp.StatusCode), "model status %d: %s", resp.StatusCode, slurp)
	}
	return json.Unmarshal(slurp, dst)
}

var (
	_ niro.VAD          = (*Speech)(nil)
	_ niro.TurnDetector = Endpoint{}
)
