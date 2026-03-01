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
// # STT — streaming (speech-to-text via WebSocket)
//
//	stt := elevenlabs.New(os.Getenv("ELEVENLABS_API_KEY"))
//	stream, err := stt.Transcribe(ctx, &niro.STTRequest{
//	    AudioStream:    micStream,          // *niro.Stream of KindAudio frames
//	    InputFormat:    niro.AudioPCM16k,
//	    InterimResults: true,
//	})
//	for stream.Next(ctx) {
//	    fmt.Print(stream.Frame().Text) // partial & final transcripts
//	}
package elevenlabs

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
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
	defaultBaseURL     = "https://api.elevenlabs.io/v1"
	defaultWSURL       = "wss://api.elevenlabs.io/v1/speech-to-text/realtime"
	defaultVoice       = "Rachel"
	defaultModel       = "eleven_multilingual_v2"
	defaultSTTModel    = "scribe_v1"
	defaultFormat      = "ogg_opus"
	readBufSize        = 8192 // 8 KB per read — matches typical TLS record
	wsHandshakeTimeout = 10 * time.Second
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

// sttWSResponse is a message received from the streaming STT WebSocket.
type sttWSResponse struct {
	MessageType string `json:"message_type"`
	Text        string `json:"text"`
	IsFinal     bool   `json:"is_final"`
}

// ── RequestHook ─────────────────────────────────────────────────────────────

// RequestHook allows modifying the raw *http.Request before it is sent.
// Use for custom headers, tracing, or per-request authentication overrides.
type RequestHook func(r *http.Request)

// ── Provider ────────────────────────────────────────────────────────────────

// Provider implements [niro.TTSProvider] and [niro.STTProvider]
// using the ElevenLabs HTTP and WebSocket APIs.
type Provider struct {
	apiKey   string
	baseURL  string
	wsURL    string
	client   *http.Client
	bp       *pool.BytePool
	voice    string
	model    string
	sttModel string
	format   string
	speed    float64
	lang     string
	hooks    []RequestHook
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

// WithSTTModel sets the default STT model.
func WithSTTModel(m string) Option {
	return func(p *Provider) { p.sttModel = m }
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
		apiKey:   apiKey,
		baseURL:  defaultBaseURL,
		wsURL:    defaultWSURL,
		client:   &http.Client{Transport: transport.Default},
		bp:       pool.DefaultBytePool,
		voice:    defaultVoice,
		model:    defaultModel,
		sttModel: defaultSTTModel,
		format:   defaultFormat,
		speed:    1.0,
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
		return nil, fmt.Errorf("niro/elevenlabs: nil request")
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, fmt.Errorf("niro/elevenlabs: empty text")
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
		return nil, fmt.Errorf("niro/elevenlabs: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("niro/elevenlabs: %w", err)
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
		return nil, fmt.Errorf("niro/elevenlabs: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, readAPIError(resp)
	}

	mime := mimeFromFormat(outFmt)
	stream, emitter := niro.NewStream(16)
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
				out.Error(fmt.Errorf("niro/elevenlabs: read: %w", err))
			}
			return
		}
	}
}

// ── STT ─────────────────────────────────────────────────────────────────────

// Transcribe implements [niro.STTProvider].
//
// Routing:
//   - If req.AudioStream is set → streaming STT via WebSocket
//     (wss://api.elevenlabs.io/v1/speech-to-text/realtime).
//     The returned Stream emits [niro.KindText] frames as partial
//     and final transcripts arrive over the connection.
//   - Otherwise → batch STT via HTTP POST /v1/speech-to-text.
//     The returned Stream emits a single [niro.KindText] frame.
//
// InterimResults controls whether partial transcripts are emitted
// (WebSocket mode only; ignored for batch).
func (p *Provider) Transcribe(ctx context.Context, req *niro.STTRequest) (*niro.Stream, error) {
	if req == nil {
		return nil, fmt.Errorf("niro/elevenlabs: nil request")
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
		return nil, fmt.Errorf("niro/elevenlabs: empty audio")
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
		return nil, fmt.Errorf("niro/elevenlabs: multipart model_id: %w", err)
	}
	if lang != "" {
		if err := w.WriteField("language_code", lang); err != nil {
			return nil, fmt.Errorf("niro/elevenlabs: multipart language_code: %w", err)
		}
	}

	ext := extFromMIME(req.InputFormat)
	part, err := w.CreateFormFile("file", "audio"+ext)
	if err != nil {
		return nil, fmt.Errorf("niro/elevenlabs: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return nil, fmt.Errorf("niro/elevenlabs: multipart file write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("niro/elevenlabs: multipart close: %w", err)
	}

	u := p.baseURL + "/speech-to-text"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return nil, fmt.Errorf("niro/elevenlabs: %w", err)
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
		return nil, fmt.Errorf("niro/elevenlabs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, readAPIError(resp)
	}

	var result sttResponse
	if err := niro.JSONNewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("niro/elevenlabs: decode: %w", err)
	}

	text := strings.TrimSpace(result.Text)
	if text == "" {
		return niro.StreamFromSlice(nil), nil
	}
	return niro.StreamFromSlice([]niro.Frame{niro.TextFrame(text)}), nil
}

// ── STT: streaming (WebSocket) ──────────────────────────────────────────────

func (p *Provider) transcribeStream(ctx context.Context, req *niro.STTRequest) (*niro.Stream, error) {
	model := req.Model
	if model == "" {
		model = p.sttModel
	}
	lang := req.Language
	if lang == "" {
		lang = p.lang
	}

	u, err := url.Parse(p.wsURL)
	if err != nil {
		return nil, fmt.Errorf("niro/elevenlabs: bad ws url: %w", err)
	}
	q := u.Query()
	q.Set("model_id", model)
	if lang != "" {
		q.Set("language_code", lang)
	}
	if req.InterimResults {
		q.Set("enable_partial_transcripts", "true")
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
		return nil, fmt.Errorf("niro/elevenlabs: ws dial: %w", err)
	}

	stream, emitter := niro.NewStream(32)
	wsCtx, wsCancel := context.WithCancel(ctx)

	// Read loop — receives transcript messages from ElevenLabs.
	go p.sttReadLoop(wsCtx, conn, emitter, req.InterimResults)

	// Write loop — sends audio frames from the input stream.
	go p.sttWriteLoop(wsCtx, wsCancel, conn, req.AudioStream, emitter)

	return stream, nil
}

// sttReadLoop reads JSON messages from the WebSocket and emits KindText frames.
func (p *Provider) sttReadLoop(ctx context.Context, conn *websocket.Conn, out *niro.Emitter, interim bool) {
	defer out.Close()
	defer conn.Close()

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
				out.Error(fmt.Errorf("niro/elevenlabs: ws read: %w", err))
			}
			return
		}

		var msg sttWSResponse
		if err := niro.JSONUnmarshal(payload, &msg); err != nil {
			continue
		}

		text := strings.TrimSpace(msg.Text)
		if text == "" {
			continue
		}

		isFinal := msg.IsFinal || isFinalMessageType(msg.MessageType)

		if !isFinal && !interim {
			continue
		}

		if emitErr := out.Emit(ctx, niro.TextFrame(text)); emitErr != nil {
			return
		}
	}
}

// sttWriteLoop reads KindAudio frames from the input stream and sends
// them as base64-encoded JSON messages over the WebSocket.
//
// The audio is base64-encoded into a pre-allocated buffer. The JSON
// envelope is assembled directly (base64 output is always JSON-safe)
// to avoid a json.Marshal round-trip per audio frame.
func (p *Provider) sttWriteLoop(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, src *niro.Stream, out *niro.Emitter) {
	defer cancel()

	// Pre-allocate base64 buffer. Typical 20ms @ 16kHz PCM frame: 640 bytes → ~856 base64.
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

		// Assemble JSON directly — base64 is always JSON-safe (no escaping needed).
		msg := make([]byte, 0, needed+20)
		msg = append(msg, `{"audio_base_64":"`...)
		msg = append(msg, b64Buf[:needed]...)
		msg = append(msg, `"}`...)

		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			if ctx.Err() == nil {
				out.Error(fmt.Errorf("niro/elevenlabs: ws write: %w", err))
			}
			return
		}
	}

	if err := src.Err(); err != nil {
		out.Error(fmt.Errorf("niro/elevenlabs: audio stream: %w", err))
		return
	}

	if err := conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
	); err != nil && ctx.Err() == nil {
		out.Error(fmt.Errorf("niro/elevenlabs: ws close: %w", err))
	}
}

// isFinalMessageType checks known ElevenLabs final transcript message types.
func isFinalMessageType(t string) bool {
	switch t {
	case "final_transcript", "committed_transcript", "transcript.final", "final":
		return true
	default:
		return false
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// readAPIError reads and formats an ElevenLabs API error response.
func readAPIError(resp *http.Response) error {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("niro/elevenlabs: status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
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
