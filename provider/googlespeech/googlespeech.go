// Package googlespeech implements [niro.TTSProvider] and [niro.STTProvider]
// backed by Google Cloud speech APIs.
//
// This module is intentionally isolated from the core and LLM providers so
// speech dependencies remain optional.
package googlespeech

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	speech "cloud.google.com/go/speech/apiv1"
	"cloud.google.com/go/speech/apiv1/speechpb"
	texttospeech "cloud.google.com/go/texttospeech/apiv1"
	"cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/option"

	"github.com/alexedtionweb/niro-stream"
)

const (
	defaultLanguage      = "en-US"
	defaultEncoding      = texttospeechpb.AudioEncoding_OGG_OPUS
	defaultChunkSize     = 32 * 1024
	defaultRequestTimout = 30 * time.Second
	defaultSTTSampleRate = 16000
	defaultGRPCPoolSize  = 2
)

var _ niro.TTSProvider = (*Provider)(nil)
var _ niro.STTProvider = (*Provider)(nil)

type ttsClient interface {
	SynthesizeSpeech(ctx context.Context, req *texttospeechpb.SynthesizeSpeechRequest, opts ...gax.CallOption) (*texttospeechpb.SynthesizeSpeechResponse, error)
	StreamingSynthesize(ctx context.Context, opts ...gax.CallOption) (streamingClient, error)
	Close() error
}

type streamingClient interface {
	Send(*texttospeechpb.StreamingSynthesizeRequest) error
	Recv() (*texttospeechpb.StreamingSynthesizeResponse, error)
	CloseSend() error
}

type sttClient interface {
	Recognize(ctx context.Context, req *speechpb.RecognizeRequest, opts ...gax.CallOption) (*speechpb.RecognizeResponse, error)
	StreamingRecognize(ctx context.Context, opts ...gax.CallOption) (sttStreamingClient, error)
	Close() error
}

type sttStreamingClient interface {
	Send(*speechpb.StreamingRecognizeRequest) error
	Recv() (*speechpb.StreamingRecognizeResponse, error)
	CloseSend() error
}

type sttClientWrapper struct {
	*speech.Client
}

func (c sttClientWrapper) StreamingRecognize(ctx context.Context, opts ...gax.CallOption) (sttStreamingClient, error) {
	return c.Client.StreamingRecognize(ctx, opts...)
}

type clientWrapper struct {
	*texttospeech.Client
}

func (c clientWrapper) StreamingSynthesize(ctx context.Context, opts ...gax.CallOption) (streamingClient, error) {
	return c.Client.StreamingSynthesize(ctx, opts...)
}

type providerConfig struct {
	clientOpts     []option.ClientOption
	client         ttsClient
	sttClient      sttClient
	sttClientOpts  []option.ClientOption
	voice          string
	language       string
	encoding       texttospeechpb.AudioEncoding
	sampleRate     int32
	speed          float64
	pitch          float64
	volumeGainDB   float64
	chunkSize      int
	requestTimeout time.Duration
	streaming      bool
}

// Provider is a Google Cloud Text-to-Speech implementation.
//
// It supports:
//   - Fast batch synthesis (SynthesizeSpeech) with chunked frame emission.
//   - Optional streaming synthesis for low-latency incremental playback.
type Provider struct {
	client         ttsClient
	sttClient      sttClient
	sttInitMu      sync.Mutex
	sttClientOpts  []option.ClientOption
	defaultVoice   string
	defaultLang    string
	defaultFormat  texttospeechpb.AudioEncoding
	defaultSR      int32
	defaultSpeed   float64
	defaultPitch   float64
	defaultVolume  float64
	chunkSize      int
	requestTimeout time.Duration
	streaming      bool
}

// Option configures a Provider.
type Option func(*providerConfig)

// WithClientOptions forwards options to texttospeech.NewClient.
func WithClientOptions(opts ...option.ClientOption) Option {
	return func(c *providerConfig) {
		c.clientOpts = append(c.clientOpts, opts...)
		c.sttClientOpts = append(c.sttClientOpts, opts...)
	}
}

// WithVoice sets the default voice name.
func WithVoice(voice string) Option {
	return func(c *providerConfig) { c.voice = strings.TrimSpace(voice) }
}

// WithLanguage sets the default BCP-47 language code.
func WithLanguage(language string) Option {
	return func(c *providerConfig) { c.language = strings.TrimSpace(language) }
}

// WithEncoding sets the default output encoding.
func WithEncoding(enc texttospeechpb.AudioEncoding) Option {
	return func(c *providerConfig) { c.encoding = enc }
}

// WithSampleRate sets the default sample rate (Hz).
func WithSampleRate(sampleRate int32) Option {
	return func(c *providerConfig) { c.sampleRate = sampleRate }
}

// WithSpeed sets the default speaking rate (1.0 is normal speed).
func WithSpeed(speed float64) Option {
	return func(c *providerConfig) { c.speed = speed }
}

// WithPitch sets the default pitch.
func WithPitch(pitch float64) Option {
	return func(c *providerConfig) { c.pitch = pitch }
}

// WithVolumeGainDB sets the default volume gain in dB.
func WithVolumeGainDB(volumeGainDB float64) Option {
	return func(c *providerConfig) { c.volumeGainDB = volumeGainDB }
}

// WithChunkSize sets output frame size for batch synthesis.
func WithChunkSize(chunkSize int) Option {
	return func(c *providerConfig) { c.chunkSize = chunkSize }
}

// WithRequestTimeout sets the per-request timeout.
func WithRequestTimeout(timeout time.Duration) Option {
	return func(c *providerConfig) { c.requestTimeout = timeout }
}

// WithStreaming enables or disables Google streaming synthesis.
func WithStreaming(enabled bool) Option {
	return func(c *providerConfig) { c.streaming = enabled }
}

// WithClient injects a custom client (used by tests and advanced integrations).
func WithClient(client ttsClient) Option {
	return func(c *providerConfig) { c.client = client }
}

// WithSTTClientOptions forwards options to speech.NewClient.
func WithSTTClientOptions(opts ...option.ClientOption) Option {
	return func(c *providerConfig) {
		c.sttClientOpts = append(c.sttClientOpts, opts...)
	}
}

// WithSTTClient injects a custom speech client (tests/advanced usage).
func WithSTTClient(client sttClient) Option {
	return func(c *providerConfig) { c.sttClient = client }
}

// New creates a Google TTS provider.
func New(ctx context.Context, opts ...Option) (*Provider, error) {
	cfg := providerConfig{
		language:       defaultLanguage,
		encoding:       defaultEncoding,
		chunkSize:      defaultChunkSize,
		requestTimeout: defaultRequestTimout,
		speed:          1.0,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if len(cfg.clientOpts) == 0 {
		cfg.clientOpts = append(cfg.clientOpts, option.WithGRPCConnectionPool(defaultGRPCPoolSize))
	}
	if len(cfg.sttClientOpts) == 0 {
		cfg.sttClientOpts = append(cfg.sttClientOpts, option.WithGRPCConnectionPool(defaultGRPCPoolSize))
	}

	p := &Provider{
		defaultVoice:   cfg.voice,
		defaultLang:    cfg.language,
		defaultFormat:  cfg.encoding,
		defaultSR:      cfg.sampleRate,
		defaultSpeed:   cfg.speed,
		defaultPitch:   cfg.pitch,
		defaultVolume:  cfg.volumeGainDB,
		chunkSize:      cfg.chunkSize,
		requestTimeout: cfg.requestTimeout,
		streaming:      cfg.streaming,
		sttClient:      cfg.sttClient,
		sttClientOpts:  append([]option.ClientOption(nil), cfg.sttClientOpts...),
	}
	if p.chunkSize <= 0 {
		p.chunkSize = defaultChunkSize
	}
	if p.requestTimeout <= 0 {
		p.requestTimeout = defaultRequestTimout
	}

	if cfg.client != nil {
		p.client = cfg.client
		return p, nil
	}

	client, err := texttospeech.NewClient(ctx, cfg.clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("niro/googlespeech: create client: %w", err)
	}
	p.client = clientWrapper{Client: client}
	return p, nil
}

// Close closes the underlying Google client.
func (p *Provider) Close() error {
	if p == nil {
		return nil
	}
	var firstErr error
	if p.client != nil {
		if err := p.client.Close(); err != nil {
			firstErr = err
		}
	}
	if p.sttClient != nil {
		if err := p.sttClient.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Synthesize implements [niro.TTSProvider].
//
// Streaming mode is best-effort. If streaming setup fails, provider falls back
// to batch synthesis automatically.
func (p *Provider) Synthesize(ctx context.Context, req *niro.TTSRequest) (*niro.Stream, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("niro/googlespeech: provider not initialized")
	}
	if req == nil {
		return nil, fmt.Errorf("niro/googlespeech: nil request")
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, fmt.Errorf("niro/googlespeech: empty text")
	}

	voice, lang, enc, sampleRate, speed, pitch, volume := p.resolveParams(req)

	stream, emitter := niro.NewStream(16)
	go func() {
		defer emitter.Close()

		callCtx, cancel := context.WithTimeout(ctx, p.requestTimeout)
		defer cancel()

		if p.streaming {
			if err := p.streamSynthesize(callCtx, emitter, text, voice, lang, enc, sampleRate, speed); err == nil {
				return
			}
		}

		if err := p.batchSynthesize(callCtx, emitter, text, voice, lang, enc, sampleRate, speed, pitch, volume); err != nil {
			emitter.Error(err)
		}
	}()
	return stream, nil
}

// Transcribe implements [niro.STTProvider].
//
// For `AudioStream`, it uses Google StreamingRecognize and emits transcript
// frames as results arrive. For single-shot `Audio`, it uses Recognize.
func (p *Provider) Transcribe(ctx context.Context, req *niro.STTRequest) (*niro.Stream, error) {
	if p == nil {
		return nil, fmt.Errorf("niro/googlespeech: provider not initialized")
	}
	if req == nil {
		return nil, fmt.Errorf("niro/googlespeech: nil request")
	}
	if req.AudioStream == nil && len(req.Audio) == 0 {
		return nil, fmt.Errorf("niro/googlespeech: empty audio")
	}

	client, err := p.ensureSTTClient(ctx)
	if err != nil {
		return nil, err
	}
	cfg := p.resolveSTTConfig(req)

	stream, emitter := niro.NewStream(32)
	go func() {
		defer emitter.Close()
		callCtx, cancel := context.WithTimeout(ctx, p.requestTimeout)
		defer cancel()

		if req.AudioStream != nil {
			if err := p.transcribeStreaming(callCtx, client, req.AudioStream, cfg, emitter); err != nil {
				emitter.Error(err)
			}
			return
		}
		if err := p.transcribeBatch(callCtx, client, req.Audio, cfg, emitter); err != nil {
			emitter.Error(err)
		}
	}()

	return stream, nil
}

func (p *Provider) batchSynthesize(
	ctx context.Context,
	emitter *niro.Emitter,
	text, voice, lang string,
	enc texttospeechpb.AudioEncoding,
	sampleRate int32,
	speed, pitch, volume float64,
) error {
	resp, err := p.client.SynthesizeSpeech(ctx, &texttospeechpb.SynthesizeSpeechRequest{
		Input: &texttospeechpb.SynthesisInput{
			InputSource: &texttospeechpb.SynthesisInput_Text{Text: text},
		},
		Voice: &texttospeechpb.VoiceSelectionParams{
			LanguageCode: lang,
			Name:         voice,
		},
		AudioConfig: &texttospeechpb.AudioConfig{
			AudioEncoding:   enc,
			SampleRateHertz: sampleRate,
			SpeakingRate:    speed,
			Pitch:           pitch,
			VolumeGainDb:    volume,
		},
	})
	if err != nil {
		return fmt.Errorf("niro/googlespeech: synthesize: %w", err)
	}

	audio := resp.GetAudioContent()
	if len(audio) == 0 {
		return nil
	}
	mime := encodingMIME(enc, sampleRate)
	for offset := 0; offset < len(audio); offset += p.chunkSize {
		end := offset + p.chunkSize
		if end > len(audio) {
			end = len(audio)
		}
		if err := emitter.Emit(ctx, niro.AudioFrame(audio[offset:end], mime)); err != nil {
			return nil
		}
	}
	return nil
}

func (p *Provider) streamSynthesize(
	ctx context.Context,
	emitter *niro.Emitter,
	text, voice, lang string,
	enc texttospeechpb.AudioEncoding,
	sampleRate int32,
	_ float64,
) error {
	if !streamingVoiceSupported(voice) {
		return fmt.Errorf("streaming unsupported for voice %q", voice)
	}
	if enc != texttospeechpb.AudioEncoding_OGG_OPUS {
		return fmt.Errorf("streaming currently requires OGG_OPUS")
	}

	stream, err := p.client.StreamingSynthesize(ctx)
	if err != nil {
		return err
	}

	if err := stream.Send(&texttospeechpb.StreamingSynthesizeRequest{
		StreamingRequest: &texttospeechpb.StreamingSynthesizeRequest_StreamingConfig{
			StreamingConfig: &texttospeechpb.StreamingSynthesizeConfig{
				Voice: &texttospeechpb.VoiceSelectionParams{
					LanguageCode: lang,
					Name:         voice,
				},
				StreamingAudioConfig: &texttospeechpb.StreamingAudioConfig{
					AudioEncoding:   enc,
					SampleRateHertz: sampleRate,
				},
			},
		},
	}); err != nil {
		_ = stream.CloseSend()
		return err
	}
	if err := stream.Send(&texttospeechpb.StreamingSynthesizeRequest{
		StreamingRequest: &texttospeechpb.StreamingSynthesizeRequest_Input{
			Input: &texttospeechpb.StreamingSynthesisInput{
				InputSource: &texttospeechpb.StreamingSynthesisInput_Text{Text: text},
			},
		},
	}); err != nil {
		_ = stream.CloseSend()
		return err
	}
	_ = stream.CloseSend()

	mime := encodingMIME(enc, sampleRate)
	for {
		resp, recvErr := stream.Recv()
		if recvErr != nil {
			if recvErr == io.EOF {
				return nil
			}
			return recvErr
		}
		audio := resp.GetAudioContent()
		if len(audio) == 0 {
			continue
		}
		if err := emitter.Emit(ctx, niro.AudioFrame(audio, mime)); err != nil {
			return nil
		}
	}
}

func (p *Provider) transcribeBatch(
	ctx context.Context,
	client sttClient,
	audio []byte,
	cfg *speechpb.RecognitionConfig,
	emitter *niro.Emitter,
) error {
	resp, err := client.Recognize(ctx, &speechpb.RecognizeRequest{
		Config: cfg,
		Audio: &speechpb.RecognitionAudio{
			AudioSource: &speechpb.RecognitionAudio_Content{
				Content: audio,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("niro/googlespeech: recognize: %w", err)
	}
	for _, result := range resp.GetResults() {
		alts := result.GetAlternatives()
		if len(alts) == 0 {
			continue
		}
		text := strings.TrimSpace(alts[0].GetTranscript())
		if text == "" {
			continue
		}
		if err := emitter.Emit(ctx, niro.TextFrame(text)); err != nil {
			return nil
		}
	}
	return nil
}

func (p *Provider) transcribeStreaming(
	ctx context.Context,
	client sttClient,
	audioStream *niro.Stream,
	cfg *speechpb.RecognitionConfig,
	emitter *niro.Emitter,
) error {
	stream, err := client.StreamingRecognize(ctx)
	if err != nil {
		return fmt.Errorf("niro/googlespeech: streaming recognize: %w", err)
	}
	if err := stream.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{
				Config:          cfg,
				InterimResults:  true,
				SingleUtterance: false,
			},
		},
	}); err != nil {
		_ = stream.CloseSend()
		return fmt.Errorf("niro/googlespeech: send stt config: %w", err)
	}

	sendErr := make(chan error, 1)
	go func() {
		defer close(sendErr)
		for audioStream.Next(ctx) {
			frame := audioStream.Frame()
			if frame.Kind != niro.KindAudio || len(frame.Data) == 0 {
				continue
			}
			if err := stream.Send(&speechpb.StreamingRecognizeRequest{
				StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
					AudioContent: frame.Data,
				},
			}); err != nil {
				sendErr <- err
				_ = stream.CloseSend()
				return
			}
		}
		if err := audioStream.Err(); err != nil {
			sendErr <- err
			_ = stream.CloseSend()
			return
		}
		_ = stream.CloseSend()
	}()

	for {
		resp, recvErr := stream.Recv()
		if recvErr != nil {
			if recvErr == io.EOF {
				if err := <-sendErr; err != nil {
					return fmt.Errorf("niro/googlespeech: send audio: %w", err)
				}
				return nil
			}
			return fmt.Errorf("niro/googlespeech: recv transcript: %w", recvErr)
		}
		for _, result := range resp.GetResults() {
			alts := result.GetAlternatives()
			if len(alts) == 0 {
				continue
			}
			text := strings.TrimSpace(alts[0].GetTranscript())
			if text == "" {
				continue
			}
			if err := emitter.Emit(ctx, niro.TextFrame(text)); err != nil {
				return nil
			}
		}
	}
}

func (p *Provider) resolveParams(req *niro.TTSRequest) (voice, lang string, enc texttospeechpb.AudioEncoding, sampleRate int32, speed, pitch, volume float64) {
	voice = fallbackString(strings.TrimSpace(req.Voice), p.defaultVoice)
	lang = fallbackString(strings.TrimSpace(req.Language), p.defaultLang)
	if lang == "" {
		lang = defaultLanguage
	}

	enc = p.defaultFormat
	if parsed := parseEncodingFromMIME(req.OutputFormat); parsed != 0 {
		enc = parsed
	}
	sampleRate = p.defaultSR
	speed = p.defaultSpeed
	pitch = p.defaultPitch
	volume = p.defaultVolume
	if req.Speed > 0 {
		speed = req.Speed
	}

	extra, _ := req.Extra.(map[string]any)
	if parsed := parseEncoding(getPayloadString(extra, "tts_encoding", "output_encoding", "encoding")); parsed != 0 {
		enc = parsed
	}
	if v := getPayloadInt(extra, "tts_sample_rate", "sample_rate"); v > 0 {
		sampleRate = int32(v)
	}
	if v := getPayloadFloat(extra, "tts_speed", "speed"); v > 0 {
		speed = v
	}
	if v := getPayloadFloat(extra, "tts_pitch", "pitch"); v != 0 {
		pitch = v
	}
	if v := getPayloadFloat(extra, "tts_volume_gain_db", "volume_gain_db"); v != 0 {
		volume = v
	}
	if v := getPayloadString(extra, "voice"); v != "" {
		voice = v
	}
	if v := getPayloadString(extra, "language", "lang"); v != "" {
		lang = v
	}
	return
}

func parseEncodingFromMIME(mime string) texttospeechpb.AudioEncoding {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case niro.AudioMP3:
		return texttospeechpb.AudioEncoding_MP3
	case "audio/ogg", niro.AudioOGGOpus:
		return texttospeechpb.AudioEncoding_OGG_OPUS
	case niro.AudioWAV:
		return texttospeechpb.AudioEncoding_LINEAR16
	case "audio/pcm", niro.AudioPCM8k, niro.AudioPCM16k, niro.AudioPCM24k, niro.AudioPCM44k, niro.AudioPCM48k:
		return texttospeechpb.AudioEncoding_PCM
	case niro.AudioPCMU8k, "audio/mulaw", "audio/g711-ulaw", "audio/ulaw":
		return texttospeechpb.AudioEncoding_MULAW
	case niro.AudioPCMA8k, "audio/alaw", "audio/g711-alaw":
		return texttospeechpb.AudioEncoding_ALAW
	default:
		return 0
	}
}

func parseSTTEncoding(inputFormat string) speechpb.RecognitionConfig_AudioEncoding {
	s := strings.ToLower(strings.TrimSpace(inputFormat))
	switch {
	case strings.Contains(s, "pcm"), strings.Contains(s, "wav"), strings.Contains(s, "linear16"):
		return speechpb.RecognitionConfig_LINEAR16
	case strings.Contains(s, "flac"):
		return speechpb.RecognitionConfig_FLAC
	case strings.Contains(s, "mp3"), strings.Contains(s, "mpeg"):
		return speechpb.RecognitionConfig_MP3
	case strings.Contains(s, "pcmu"), strings.Contains(s, "mulaw"), strings.Contains(s, "g711-ulaw"), strings.Contains(s, "ulaw"):
		return speechpb.RecognitionConfig_MULAW
	case strings.Contains(s, "pcma"), strings.Contains(s, "alaw"), strings.Contains(s, "g711-alaw"):
		// STT v1 has no A-law enum; normalize upstream or transcode to LINEAR16.
		return speechpb.RecognitionConfig_LINEAR16
	case strings.Contains(s, "amr-wb"):
		return speechpb.RecognitionConfig_AMR_WB
	case strings.Contains(s, "amr"):
		return speechpb.RecognitionConfig_AMR
	case strings.Contains(s, "speex"), strings.Contains(s, "spx"):
		return speechpb.RecognitionConfig_SPEEX_WITH_HEADER_BYTE
	case strings.Contains(s, "ogg"), strings.Contains(s, "opus"):
		return speechpb.RecognitionConfig_OGG_OPUS
	case strings.Contains(s, "webm"):
		return speechpb.RecognitionConfig_WEBM_OPUS
	default:
		return speechpb.RecognitionConfig_LINEAR16
	}
}

func parseSTTSampleRate(inputFormat string) int {
	s := strings.ToLower(strings.TrimSpace(inputFormat))
	if idx := strings.Index(s, "rate="); idx >= 0 {
		start := idx + len("rate=")
		end := start
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
		}
		if end > start {
			if rate, err := strconv.Atoi(s[start:end]); err == nil && rate > 0 {
				return rate
			}
		}
	}
	return defaultSTTSampleRate
}

func parseEncoding(enc string) texttospeechpb.AudioEncoding {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "mp3":
		return texttospeechpb.AudioEncoding_MP3
	case "ogg_opus", "ogg", "opus":
		return texttospeechpb.AudioEncoding_OGG_OPUS
	case "pcm":
		return texttospeechpb.AudioEncoding_PCM
	case "linear16", "wav":
		return texttospeechpb.AudioEncoding_LINEAR16
	case "mulaw":
		return texttospeechpb.AudioEncoding_MULAW
	case "alaw":
		return texttospeechpb.AudioEncoding_ALAW
	default:
		return 0
	}
}

func encodingMIME(enc texttospeechpb.AudioEncoding, sampleRate int32) string {
	switch enc {
	case texttospeechpb.AudioEncoding_MP3:
		return niro.AudioMP3
	case texttospeechpb.AudioEncoding_LINEAR16:
		return niro.AudioWAV
	case texttospeechpb.AudioEncoding_OGG_OPUS:
		return niro.AudioOGGOpus
	case texttospeechpb.AudioEncoding_PCM:
		switch sampleRate {
		case 8000:
			return niro.AudioPCM8k
		case 16000:
			return niro.AudioPCM16k
		case 24000:
			return niro.AudioPCM24k
		case 44100:
			return niro.AudioPCM44k
		case 48000:
			return niro.AudioPCM48k
		default:
			if sampleRate > 0 {
				return fmt.Sprintf("audio/pcm;rate=%d;bits=16;channels=1", sampleRate)
			}
			return niro.AudioPCM16k
		}
	case texttospeechpb.AudioEncoding_MULAW:
		return niro.AudioPCMU8k
	case texttospeechpb.AudioEncoding_ALAW:
		return niro.AudioPCMA8k
	default:
		return niro.AudioOGGOpus
	}
}

func streamingVoiceSupported(voice string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(voice)), "chirp3-hd")
}

func getPayloadString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key]; ok {
			if s, ok := value.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					return s
				}
			}
		}
	}
	if nested, ok := payload["tts"].(map[string]any); ok {
		for _, key := range keys {
			if value, ok := nested[key]; ok {
				if s, ok := value.(string); ok {
					s = strings.TrimSpace(s)
					if s != "" {
						return s
					}
				}
			}
		}
	}
	return ""
}

func getPayloadFloat(payload map[string]any, keys ...string) float64 {
	for _, key := range keys {
		if value, ok := payload[key]; ok {
			if number, ok := asFloat(value); ok {
				return number
			}
		}
	}
	if nested, ok := payload["tts"].(map[string]any); ok {
		for _, key := range keys {
			if value, ok := nested[key]; ok {
				if number, ok := asFloat(value); ok {
					return number
				}
			}
		}
	}
	return 0
}

func getPayloadInt(payload map[string]any, keys ...string) int {
	for _, key := range keys {
		if value, ok := payload[key]; ok {
			if number, ok := asInt(value); ok {
				return number
			}
		}
	}
	if nested, ok := payload["tts"].(map[string]any); ok {
		for _, key := range keys {
			if value, ok := nested[key]; ok {
				if number, ok := asInt(value); ok {
					return number
				}
			}
		}
	}
	return 0
}

func asFloat(v any) (float64, bool) {
	switch number := v.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int32:
		return float64(number), true
	case int64:
		return float64(number), true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func asInt(v any) (int, bool) {
	switch number := v.(type) {
	case int:
		return number, true
	case int32:
		return int(number), true
	case int64:
		return int(number), true
	case float64:
		return int(number), true
	case float32:
		return int(number), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(number))
		return parsed, err == nil
	default:
		return 0, false
	}
}

func fallbackString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func (p *Provider) resolveSTTConfig(req *niro.STTRequest) *speechpb.RecognitionConfig {
	lang := strings.TrimSpace(req.Language)
	if lang == "" {
		lang = p.defaultLang
	}
	if lang == "" {
		lang = defaultLanguage
	}

	sampleRate := parseSTTSampleRate(req.InputFormat)
	encoding := parseSTTEncoding(req.InputFormat)
	model := strings.TrimSpace(req.Model)

	extra, _ := req.Extra.(map[string]any)
	if v := getPayloadString(extra, "language", "lang"); v != "" {
		lang = v
	}
	if v := getPayloadInt(extra, "sample_rate", "stt_sample_rate"); v > 0 {
		sampleRate = v
	}
	if v := getPayloadString(extra, "encoding", "stt_encoding"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "linear16", "pcm", "wav":
			encoding = speechpb.RecognitionConfig_LINEAR16
		case "flac":
			encoding = speechpb.RecognitionConfig_FLAC
		case "mp3":
			encoding = speechpb.RecognitionConfig_MP3
		case "mulaw":
			encoding = speechpb.RecognitionConfig_MULAW
		case "alaw":
			encoding = speechpb.RecognitionConfig_LINEAR16
		case "amr":
			encoding = speechpb.RecognitionConfig_AMR
		case "amr_wb", "amr-wb":
			encoding = speechpb.RecognitionConfig_AMR_WB
		case "speex", "speex_with_header_byte":
			encoding = speechpb.RecognitionConfig_SPEEX_WITH_HEADER_BYTE
		case "ogg_opus", "ogg", "opus":
			encoding = speechpb.RecognitionConfig_OGG_OPUS
		case "webm_opus", "webm":
			encoding = speechpb.RecognitionConfig_WEBM_OPUS
		}
	}
	if v := getPayloadString(extra, "model", "stt_model"); v != "" {
		model = v
	}

	cfg := &speechpb.RecognitionConfig{
		Encoding:        encoding,
		SampleRateHertz: int32(sampleRate),
		LanguageCode:    lang,
	}
	if model != "" {
		cfg.Model = model
	}
	return cfg
}

func (p *Provider) ensureSTTClient(ctx context.Context) (sttClient, error) {
	if p.sttClient != nil {
		return p.sttClient, nil
	}

	p.sttInitMu.Lock()
	defer p.sttInitMu.Unlock()
	if p.sttClient != nil {
		return p.sttClient, nil
	}
	client, err := speech.NewClient(ctx, p.sttClientOpts...)
	if err != nil {
		return nil, fmt.Errorf("niro/googlespeech: create speech client: %w", err)
	}
	p.sttClient = sttClientWrapper{Client: client}
	return p.sttClient, nil
}
