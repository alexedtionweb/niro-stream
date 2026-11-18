// Package elevenlabs implements [niro.TTSProvider] and [niro.STTProvider]
// backed by the ElevenLabs HTTP and WebSocket APIs.
//
// The provider uses a shared high-performance [transport.Default] HTTP
// transport with connection pooling.
//
// # TTS (text-to-speech)
//
//	tts := elevenlabs.New(os.Getenv("ELEVENLABS_API_KEY"))
//	stream, err := tts.Synthesize(ctx, &niro.TTSRequest{
//	    Text:  "Hello world",
//	    Voice: "Rachel",
//	})
//	for stream.Next(ctx) {
//	    speaker.Write(stream.Frame().Data) // audio chunks
//	}
//
// # STT — batch (speech-to-text via HTTP)
//
//	stt := elevenlabs.New(os.Getenv("ELEVENLABS_API_KEY"))
//	stream, err := stt.Transcribe(ctx, &niro.STTRequest{
//	    Audio:       wavBytes,
//	    InputFormat: niro.AudioWAV,
//	    Language:    "en",
//	})
//	for stream.Next(ctx) {
//	    fmt.Print(stream.Frame().Text)
//	}
//
// # STT — streaming via Scribe realtime (WebSocket)
//
// Uses wss://api.elevenlabs.io/v1/speech-to-text/realtime with the Scribe
// realtime models (default [defaultSTTStreamModel]). Audio is sent as
// input_audio_chunk events; partial_transcript / committed_transcript
// arrive as KindText frames with [niro.STTMeta] on Frame.Custom.
//
//	stt := elevenlabs.New(os.Getenv("ELEVENLABS_API_KEY"))
//	stream, err := stt.Transcribe(ctx, &niro.STTRequest{
//	    AudioStream:    micStream, // *niro.Stream of KindAudio frames
//	    InputFormat:    niro.AudioPCM16k,
//	    InterimResults: true,
//	})
//	for stream.Next(ctx) {
//	    fr := stream.Frame()
//	    if fr.Kind == niro.KindText {
//	        fmt.Print(fr.Text)
//	    }
//	}
package elevenlabs

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/pool"
	"github.com/alexedtionweb/niro-stream/transport"
)

const (
	defaultBaseURL        = "https://api.elevenlabs.io/v1"
	defaultWSURL          = "wss://api.elevenlabs.io/v1/speech-to-text/realtime"
	defaultVoice          = "Rachel"
	defaultModel          = "eleven_multilingual_v2"
	defaultSTTModel       = "scribe_v1"           // batch HTTP
	defaultSTTStreamModel = "scribe_v2_realtime"  // Scribe realtime WebSocket
	defaultCommitStrategy = "vad"                 // auto-segment for streaming
	defaultFormat         = "ogg_opus"
	readBufSize           = 8192 // 8 KB per read — matches typical TLS record
	wsHandshakeTimeout    = 10 * time.Second
)

// ── TTS request body ────────────────────────────────────────────────────────

// ttsBody is the JSON body for POST /text-to-speech/{voice}/stream.
type ttsBody struct {
	Text          string        `json:"text"`
	ModelID       string        `json:"model_id"`
	LanguageCode  string        `json:"language_code,omitempty"`
	VoiceSettings voiceSettings `json:"voice_settings"`
}

type voiceSettings struct {
	Speed float64 `json:"speed"`
}

// ttsBody pool — avoids allocation per Synthesize call on hot paths.
var ttsBodyPool = sync.Pool{
	New: func() any { return &ttsBody{} },
}

func acquireTTSBody(text, model, lang string, speed float64) *ttsBody {
	b := ttsBodyPool.Get().(*ttsBody)
	b.Text = text
	b.ModelID = model
	b.LanguageCode = lang
	b.VoiceSettings.Speed = speed
	return b
}

func releaseTTSBody(b *ttsBody) {
	b.Text = ""
	b.ModelID = ""
	b.LanguageCode = ""
	b.VoiceSettings.Speed = 0
	ttsBodyPool.Put(b)
}

// ── STT response types ──────────────────────────────────────────────────────

// sttResponse is the JSON response from POST /speech-to-text (batch).
type sttResponse struct {
	Text string `json:"text"`
}

// sttWSResponse is a message received from the Scribe realtime WebSocket.
type sttWSResponse struct {
	MessageType  string `json:"message_type"`
	Text         string `json:"text"`
	LanguageCode string `json:"language_code"`
	Error        string `json:"error"`
	// Legacy / alternate fields kept for test fixtures and older gateways.
	IsFinal bool `json:"is_final"`
}

// sttWSAudioChunk is sent to Scribe realtime as input_audio_chunk.
type sttWSAudioChunk struct {
	MessageType string `json:"message_type"`
	AudioBase64 string `json:"audio_base_64"`
	Commit      bool   `json:"commit"`
	SampleRate  int    `json:"sample_rate,omitempty"`
}

// ── RequestHook ─────────────────────────────────────────────────────────────

// RequestHook allows modifying the raw *http.Request before it is sent.
// Use for custom headers, tracing, or per-request authentication overrides.
type RequestHook func(r *http.Request)

// ── Provider ────────────────────────────────────────────────────────────────

// Provider implements [niro.TTSProvider] and [niro.STTProvider]
// using the ElevenLabs HTTP and WebSocket APIs.
type Provider struct {
	apiKey         string
	baseURL        string
	wsURL          string
	client         *http.Client
	bp             *pool.BytePool
	voice          string
	model          string
	sttModel       string
	sttStreamModel string
	commitStrategy string
	format         string
	speed          float64
	lang           string
	hooks          []RequestHook
}

// compile-time interface checks
var (
	_ niro.TTSProvider = (*Provider)(nil)
	_ niro.STTProvider = (*Provider)(nil)
)

// Option configures a [Provider].
type Option func(*Provider)

// WithBaseURL overrides the HTTP API base URL.
func WithBaseURL(u string) Option {
	return func(p *Provider) { p.baseURL = strings.TrimRight(u, "/") }
}

// WithWSURL overrides the WebSocket URL for streaming STT.
func WithWSURL(u string) Option {
	return func(p *Provider) { p.wsURL = u }
}

// WithHTTPClient replaces the default HTTP client.
// The default uses [transport.Default] which is tuned for streaming.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) { p.client = c }
}

// WithBytePool replaces the default byte pool.
// Use when you need isolation from the process-wide pool.
func WithBytePool(bp *pool.BytePool) Option {
	return func(p *Provider) { p.bp = bp }
}

// WithVoice sets the default synthesis voice.
func WithVoice(v string) Option {
	return func(p *Provider) { p.voice = v }
}

// WithModel sets the default TTS model.
func WithModel(m string) Option {
	return func(p *Provider) { p.model = m }
}

// WithSTTModel sets the default batch STT model (HTTP /speech-to-text).
func WithSTTModel(m string) Option {
	return func(p *Provider) { p.sttModel = m }
}

// WithSTTStreamModel sets the default Scribe realtime model
// (e.g. "scribe_v2_realtime", "scribe_v3_live").
func WithSTTStreamModel(m string) Option {
	return func(p *Provider) { p.sttStreamModel = m }
}

// WithCommitStrategy sets the Scribe realtime commit strategy:
// "manual", "vad" (default), or "turn_prediction".
func WithCommitStrategy(s string) Option {
	return func(p *Provider) { p.commitStrategy = s }
}

// WithFormat sets the default output format (e.g. "ogg_opus", "mp3_44100_128").
func WithFormat(f string) Option {
	return func(p *Provider) { p.format = f }
}

// WithSpeed sets the default speech speed multiplier.
func WithSpeed(s float64) Option {
	return func(p *Provider) { p.speed = s }
}

// WithLanguage sets the default BCP-47 language code.
func WithLanguage(lang string) Option {
	return func(p *Provider) { p.lang = lang }
}

// WithRequestHook registers a function called with the raw *http.Request
// before each HTTP API call. Multiple hooks are called in registration order.
func WithRequestHook(fn RequestHook) Option {
	return func(p *Provider) { p.hooks = append(p.hooks, fn) }
}

// New creates an ElevenLabs provider.
//
//	tts := elevenlabs.New(os.Getenv("ELEVENLABS_API_KEY"),
//	    elevenlabs.WithVoice("Rachel"),
//	    elevenlabs.WithFormat("mp3_44100_128"),
//	)
func New(apiKey string, opts ...Option) *Provider {
	p := &Provider{
		apiKey:         apiKey,
		baseURL:        defaultBaseURL,
		wsURL:          defaultWSURL,
		client:         &http.Client{Transport: transport.Default},
		bp:             pool.DefaultBytePool,
		voice:          defaultVoice,
		model:          defaultModel,
		sttModel:       defaultSTTModel,
		sttStreamModel: defaultSTTStreamModel,
		commitStrategy: defaultCommitStrategy,
		format:         defaultFormat,
		speed:          1.0,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// ── TTS ─────────────────────────────────────────────────────────────────────

// Synthesize implements [niro.TTSProvider].
//
// The returned Stream emits [niro.KindAudio] frames with pooled byte
// buffers read directly from the HTTP response body. Callers that need
// to hold onto frame data beyond the stream iteration must copy it.
//
// Context cancellation aborts the HTTP request immediately.
func (p *Provider) Synthesize(ctx context.Context, req *niro.TTSRequest) (*niro.Stream, error) {
	if req == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "nil request")
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "empty text")
	}

	voice := req.Voice
	if voice == "" {
		voice = p.voice
	}
	model := req.Model
	if model == "" {
		model = p.model
	}
	lang := req.Language
	if lang == "" {
		lang = p.lang
	}
	outFmt := outputFormatFromMIME(req.OutputFormat)
	if outFmt == "" {
		outFmt = p.format
	}
	speed := req.Speed
	if speed <= 0 {
		speed = p.speed
	}

	// Build URL: /text-to-speech/{voice_id}/stream?output_format=...
	u := p.baseURL + "/text-to-speech/" + url.PathEscape(voice) + "/stream"
	if outFmt != "" {
		u += "?output_format=" + url.QueryEscape(outFmt)
	}

	// Marshal JSON body using a pooled struct and niro's pluggable JSON backend.
	tb := acquireTTSBody(text, model, lang, speed)
	raw, err := niro.JSONMarshal(tb)
	releaseTTSBody(tb)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "marshal", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "request", err)
	}
	httpReq.Header.Set("xi-api-key", p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "*/*")
	httpReq.ContentLength = int64(len(raw))

	for _, h := range p.hooks {
		h(httpReq)
	}
	if hook, ok := req.Extra.(RequestHook); ok {
		hook(httpReq)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "do", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, readAPIError(resp)
	}

	mime := mimeFromFormat(outFmt)
	stream, emitter := niro.NewStream(niro.DefaultStreamBuffer)
	go p.consumeTTS(ctx, resp.Body, emitter, mime)
	return stream, nil
}

// consumeTTS reads audio chunks from the HTTP body and emits them as
// KindAudio frames. Each emitted frame owns its []byte so downstream
// consumers can retain data safely.
func (p *Provider) consumeTTS(ctx context.Context, body io.ReadCloser, out *niro.Emitter, mime string) {
	defer out.Close()
	defer body.Close()

	for {
		buf := p.bp.Get(readBufSize)
		n, err := body.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			p.bp.Put(buf)

			f := niro.Frame{Kind: niro.KindAudio, Data: data, Mime: mime}
			if emitErr := out.Emit(ctx, f); emitErr != nil {
				return
			}
		} else {
			p.bp.Put(buf)
		}
		if err != nil {
			if err != io.EOF {
				out.Error(niro.WrapError(niro.ErrCodeStreamError, "read", err))
			}
			return
		}
	}
}

// ── STT ─────────────────────────────────────────────────────────────────────

// Transcribe implements [niro.STTProvider].
//
// Routing:
//   - If req.AudioStream is set → Scribe realtime WebSocket
//     (wss://api.elevenlabs.io/v1/speech-to-text/realtime).
//     Emits [niro.KindText] frames; [niro.STTMeta] is attached on
//     Frame.Custom (Type [niro.CustomSTTMeta]).
//   - Otherwise → batch STT via HTTP POST /v1/speech-to-text.
//     Emits a single final [niro.KindText] frame.
//
// InterimResults controls whether partial_transcript events are emitted
// (WebSocket mode only; ignored for batch).
func (p *Provider) Transcribe(ctx context.Context, req *niro.STTRequest) (*niro.Stream, error) {
	if req == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "nil request")
	}
	if req.AudioStream != nil {
		return p.transcribeStream(ctx, req)
	}
	return p.transcribeBatch(ctx, req)
}

// ── STT: batch (HTTP) ───────────────────────────────────────────────────────

func (p *Provider) transcribeBatch(ctx context.Context, req *niro.STTRequest) (*niro.Stream, error) {
	audio := req.Audio
	if len(audio) == 0 {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "empty audio")
	}

	model := req.Model
	if model == "" {
		model = p.sttModel
	}
	lang := req.Language
	if lang == "" {
		lang = p.lang
	}

	// Pre-size buffer: ~300 bytes multipart framing + audio payload.
	var body bytes.Buffer
	body.Grow(300 + len(audio))
	w := multipart.NewWriter(&body)

	if err := w.WriteField("model_id", model); err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "multipart model_id", err)
	}
	if lang != "" {
		if err := w.WriteField("language_code", lang); err != nil {
			return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "multipart language_code", err)
		}
	}

	ext := extFromMIME(req.InputFormat)
	part, err := w.CreateFormFile("file", "audio"+ext)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "do", err)
	}
	if _, err := part.Write(audio); err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "multipart file write", err)
	}
	if err := w.Close(); err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "multipart close", err)
	}

	u := p.baseURL + "/speech-to-text"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "do", err)
	}
	httpReq.Header.Set("xi-api-key", p.apiKey)
	httpReq.Header.Set("Content-Type", w.FormDataContentType())
	httpReq.ContentLength = int64(body.Len())

	for _, h := range p.hooks {
		h(httpReq)
	}
	if hook, ok := req.Extra.(RequestHook); ok {
		hook(httpReq)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "do", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, readAPIError(resp)
	}

	var result sttResponse
	if err := niro.JSONNewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "decode", err)
	}

	text := strings.TrimSpace(result.Text)
	if text == "" {
		return niro.StreamFromSlice(nil), nil
	}
	return niro.StreamFromSlice([]niro.Frame{niro.TextFrame(text)}), nil
}

// ── STT: Scribe realtime (WebSocket) ────────────────────────────────────────

func (p *Provider) transcribeStream(ctx context.Context, req *niro.STTRequest) (*niro.Stream, error) {
	model := req.Model
	if model == "" {
		model = p.sttStreamModel
	}
	lang := req.Language
	if lang == "" {
		lang = p.lang
	}
	commitStrategy := p.commitStrategy
	if commitStrategy == "" {
		commitStrategy = defaultCommitStrategy
	}
	audioFormat, sampleRate := scribeAudioParams(req.InputFormat)

	u, err := url.Parse(p.wsURL)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "bad ws url", err)
	}
	q := u.Query()
	q.Set("model_id", model)
	if lang != "" {
		q.Set("language_code", lang)
	}
	if audioFormat != "" {
		q.Set("audio_format", audioFormat)
	}
	if commitStrategy != "" {
		q.Set("commit_strategy", commitStrategy)
	}
	u.RawQuery = q.Encode()

	header := http.Header{}
	header.Set("xi-api-key", p.apiKey)

	handshakeTimeout := p.client.Timeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = wsHandshakeTimeout
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
	}
	conn, _, err := dialer.DialContext(ctx, u.String(), header)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "ws dial", err)
	}

	stream, emitter := niro.NewStream(niro.DefaultStreamBuffer)
	wsCtx, wsCancel := context.WithCancel(ctx)
	sessionStarted := make(chan struct{})

	go p.sttReadLoop(wsCtx, conn, emitter, req.InterimResults, sessionStarted)
	go p.sttWriteLoop(wsCtx, wsCancel, conn, req.AudioStream, emitter, sampleRate, sessionStarted)

	return stream, nil
}

// sttReadLoop reads Scribe realtime events and emits KindText frames.
func (p *Provider) sttReadLoop(ctx context.Context, conn *websocket.Conn, out *niro.Emitter, interim bool, sessionStarted chan struct{}) {
	defer out.Close()
	defer conn.Close()

	startedOnce := sync.Once{}
	defer startedOnce.Do(func() { close(sessionStarted) })

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, payload, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() == nil && !websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
			) {
				out.Error(niro.WrapError(niro.ErrCodeStreamError, "ws read", err))
			}
			return
		}

		var msg sttWSResponse
		if err := niro.JSONUnmarshal(payload, &msg); err != nil {
			continue
		}

		switch msg.MessageType {
		case "session_started":
			startedOnce.Do(func() { close(sessionStarted) })
			continue

		case "partial_transcript", "partial":
			text := strings.TrimSpace(msg.Text)
			if text == "" || !interim {
				continue
			}
			if err := out.Emit(ctx, sttTextFrame(text, false, msg.LanguageCode)); err != nil {
				return
			}

		case "committed_transcript", "final_transcript", "transcript.final", "final":
			text := strings.TrimSpace(msg.Text)
			if text == "" {
				continue
			}
			if err := out.Emit(ctx, sttTextFrame(text, true, msg.LanguageCode)); err != nil {
				return
			}

		case "auth_error", "quota_exceeded", "transcriber_error", "input_error",
			"invalid_request", "error", "rate_limited", "unaccepted_terms",
			"queue_overflow", "resource_exhausted", "session_time_limit_exceeded",
			"chunk_size_exceeded", "insufficient_audio_activity", "commit_throttled":
			errMsg := strings.TrimSpace(msg.Error)
			if errMsg == "" {
				errMsg = msg.MessageType
			}
			out.Error(niro.NewError(niro.ErrCodeProviderError, "scribe: "+errMsg).WithProvider("elevenlabs"))
			return

		default:
			// Ignore timestamps/entities/edited/diarized extras and keepalives.
			if msg.IsFinal {
				text := strings.TrimSpace(msg.Text)
				if text == "" {
					continue
				}
				if err := out.Emit(ctx, sttTextFrame(text, true, msg.LanguageCode)); err != nil {
					return
				}
			}
		}
	}
}

// sttWriteLoop sends Scribe input_audio_chunk events, then a final commit.
func (p *Provider) sttWriteLoop(
	ctx context.Context,
	cancel context.CancelFunc,
	conn *websocket.Conn,
	src *niro.Stream,
	out *niro.Emitter,
	sampleRate int,
	sessionStarted <-chan struct{},
) {
	// Only cancel on hard write failures. A successful commit must leave the
	// read loop alive so committed_transcript events can still arrive.
	fail := func(err error) {
		out.Error(err)
		cancel()
	}

	select {
	case <-ctx.Done():
		return
	case <-sessionStarted:
	case <-time.After(wsHandshakeTimeout):
		// Proceed anyway — some test mocks never send session_started.
	}

	b64Buf := make([]byte, base64.StdEncoding.EncodedLen(readBufSize))

	for src.Next(ctx) {
		f := src.Frame()
		if f.Kind != niro.KindAudio || len(f.Data) == 0 {
			continue
		}

		needed := base64.StdEncoding.EncodedLen(len(f.Data))
		if needed > len(b64Buf) {
			b64Buf = make([]byte, needed)
		}
		base64.StdEncoding.Encode(b64Buf[:needed], f.Data)

		msg := sttWSAudioChunk{
			MessageType: "input_audio_chunk",
			AudioBase64: string(b64Buf[:needed]),
			Commit:      false,
			SampleRate:  sampleRate,
		}
		raw, err := niro.JSONMarshal(msg)
		if err != nil {
			fail(niro.WrapError(niro.ErrCodeStreamError, "marshal", err))
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
			if ctx.Err() == nil {
				fail(niro.WrapError(niro.ErrCodeStreamError, "ws write", err))
			}
			return
		}
	}

	if err := src.Err(); err != nil {
		fail(niro.WrapError(niro.ErrCodeStreamError, "audio stream", err))
		return
	}

	// Final commit flushes any remaining uncommitted audio (manual + vad).
	// Read loop owns conn lifetime so committed_transcript can still arrive.
	commit, err := niro.JSONMarshal(sttWSAudioChunk{
		MessageType: "input_audio_chunk",
		AudioBase64: "",
		Commit:      true,
		SampleRate:  sampleRate,
	})
	if err != nil {
		fail(niro.WrapError(niro.ErrCodeStreamError, "marshal", err))
		return
	}
	if err := conn.WriteMessage(websocket.TextMessage, commit); err != nil && ctx.Err() == nil {
		fail(niro.WrapError(niro.ErrCodeStreamError, "ws commit", err))
	}
}

func sttTextFrame(text string, final bool, lang string) niro.Frame {
	f := niro.TextFrame(text)
	f.Custom = &niro.ExperimentalFrame{
		Type: niro.CustomSTTMeta,
		Data: niro.STTMeta{Final: final, Language: lang},
	}
	return f
}

// scribeAudioParams maps a niro audio MIME to Scribe audio_format + sample_rate.
func scribeAudioParams(mime string) (format string, sampleRate int) {
	switch mime {
	case niro.AudioPCM16k:
		return "pcm_16000", 16000
	case niro.AudioPCM24k:
		return "pcm_24000", 24000
	case niro.AudioPCM44k:
		return "pcm_44100", 44100
	case niro.AudioPCMU8k:
		return "ulaw_8000", 8000
	case niro.AudioPCMA8k:
		return "pcm_8000", 8000
	default:
		// Safe default for realtime mic capture.
		return "pcm_16000", 16000
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// readAPIError reads and formats an ElevenLabs API error response.
func readAPIError(resp *http.Response) error {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return niro.NewErrorf(niro.ConvertHTTPStatusToCode(resp.StatusCode), "status %d: %s", resp.StatusCode, string(bytes.TrimSpace(b)))
}

// mimeFromFormat converts an ElevenLabs output_format parameter to a MIME type.
func mimeFromFormat(f string) string {
	v := strings.ToLower(f)
	switch {
	case strings.HasPrefix(v, "ogg") || strings.Contains(v, "opus"):
		return niro.AudioOGGOpus
	case strings.HasPrefix(v, "mp3") || strings.Contains(v, "mpeg"):
		return niro.AudioMP3
	case strings.HasPrefix(v, "pcm"):
		return niro.AudioPCM24k
	case strings.HasPrefix(v, "ulaw"):
		return "audio/basic"
	default:
		return niro.AudioOGGOpus
	}
}

// outputFormatFromMIME converts a MIME type to an ElevenLabs output_format param.
func outputFormatFromMIME(mime string) string {
	switch mime {
	case niro.AudioOGGOpus:
		return "ogg_opus"
	case niro.AudioMP3:
		return "mp3_44100_128"
	case niro.AudioAAC:
		return "aac_44100"
	case niro.AudioPCM16k:
		return "pcm_16000"
	case niro.AudioPCM24k:
		return "pcm_24000"
	case niro.AudioPCM44k:
		return "pcm_44100"
	case niro.AudioFLAC:
		return "flac"
	case niro.AudioWAV:
		return "wav"
	case "":
		return ""
	default:
		return ""
	}
}

// extFromMIME returns a file extension for the given MIME type.
func extFromMIME(mime string) string {
	switch {
	case strings.Contains(mime, "ogg") || strings.Contains(mime, "opus"):
		return ".ogg"
	case strings.Contains(mime, "mpeg") || strings.Contains(mime, "mp3"):
		return ".mp3"
	case strings.Contains(mime, "wav"):
		return ".wav"
	case strings.Contains(mime, "flac"):
		return ".flac"
	case strings.Contains(mime, "pcm"):
		return ".raw"
	default:
		return ".bin"
	}
}
