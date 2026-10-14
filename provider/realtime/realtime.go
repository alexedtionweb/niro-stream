// Package realtime implements a Niro RealtimeProvider for the OpenAI Realtime
// API and for vLLM-Omni's OpenAI-compatible /v1/realtime endpoint.
//
// Model, voice, and audio format are sent only when you set them. An empty
// field is omitted so the server keeps its own default.
//
//	p := realtime.NewOmni(os.Getenv("VLLM_BASE_URL"), realtime.WithModel(os.Getenv("MODEL")))
//	sess, err := p.Session(ctx, niro.RealtimeConfig{
//	    SystemPrompt: "You are a helpful voice assistant.",
//	    InputFormat:  niro.AudioPCM16k, // omit to let the server choose
//	})
//
// The audio append path builds JSON by hand and hands the buffer to the
// write loop, so a steady 20 ms mic cadence does not allocate after warmup.
// Incoming audio deltas are scanned without encoding/json and base64-decoded
// straight into the PCM frame.
//
// WithBeta selects the legacy preview protocol (OpenAI-Beta: realtime=v1).
// GA event names and the legacy response.audio.delta spelling are both accepted.
package realtime

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/alexedtionweb/niro-stream"
)

const (
	defaultEndpoint  = "wss://api.openai.com/v1/realtime"
	writeWait        = 10 * time.Second
	handshakeTimeout = 10 * time.Second
	sendQueue        = 8
	ackEvery         = 32
	maxResume        = 5
	maxVideoFrames   = 8

	// EventSession is emitted once, on session.created.
	// Frame.Custom.Data is a [SessionInfo].
	EventSession = "realtime.session"
	// EventSpeak is a vLLM-Omni model-native decision to speak.
	// Frame.Custom.Data is the response id.
	EventSpeak = "realtime.speak"
	// EventListen is a vLLM-Omni model-native decision to keep listening.
	// Frame.Custom.Data is the response id (may be empty).
	EventListen = "realtime.listen"
	// EventInputTranscript is the user's audio transcript.
	// Frame.Text holds the transcript.
	EventInputTranscript = "input_transcript"
	// EventResponseFailed is a failed turn that does not close the session.
	// Frame.Custom.Data is the reason string.
	EventResponseFailed = "response.failed"
	// EventOverlap is a vLLM-Omni overlap.decision.
	EventOverlap = "overlap.decision"
	// EventPlaybackAck is sent by the caller to report how much assistant
	// audio was actually played. Frame.Custom.Data is a [PlaybackAck].
	EventPlaybackAck = "playback.ack"
	// EventPlaybackAcked is the server's playback.acknowledged reply.
	EventPlaybackAcked = "playback.acknowledged"
	// EventBargeIn asks a duplex server for a hard interrupt.
	EventBargeIn = "barge_in"
)

type dialect uint8

const (
	dialectOpenAI dialect = iota
	dialectVLLM
)

// Protocol selects the session handshake.
type Protocol uint8

const (
	// ProtocolOpenAI is the GA Realtime API.
	ProtocolOpenAI Protocol = iota
	// ProtocolVLLMOmni is vLLM-Omni /v1/realtime (turn-based or duplex).
	ProtocolVLLMOmni
)

// SessionConfig is passed as [niro.RealtimeConfig.Extra].
type SessionConfig struct {
	// ModelNative leaves turn-taking to a duplex model. End-of-turn commits
	// audio and does not send response.create.
	ModelNative bool
	// ExplicitResponse forces response.create on end-of-turn even after the
	// server reports a model-native session.
	ExplicitResponse bool

	// RefAudio is a data URL or base64 clip used as the assistant voice reference
	// when the served model requires one.
	RefAudio string
	// OverlapPolicy is listen_only, barge_in_on_speech, or a server default.
	OverlapPolicy string
	// PlaybackCommitPolicy is ack_only or commit_all_on_done.
	PlaybackCommitPolicy string
	IdleTimeoutS         float64
	// AutoResponse overrides the vLLM extra_body.auto_response flag.
	// Nil sends false so a single response.create starts the turn.
	AutoResponse *bool
	ForceListen  *int
	ExtraBody    map[string]any

	// TranscriptionModel enables OpenAI input transcription
	// (for example "gpt-4o-mini-transcribe"). Empty keeps it off.
	TranscriptionModel string
	// NoiseReduction is "near_field" or "far_field".
	NoiseReduction string
	// OutputModalities defaults to ["audio"] on OpenAI.
	OutputModalities []string
	Speed            *float64
}

// SessionInfo is the payload of [EventSession].
type SessionInfo struct {
	ID              string
	ResumeToken     string
	SupportsBargeIn bool
	SupportsResume  bool
	ChunkPeriodMs   int
	InputRate       int
	OutputRate      int
}

// PlaybackAck reports cumulative playback of one assistant response.
// With playback_commit_policy=ack_only the server only keeps history up to
// CommittedMs, which is what makes a barge-in honest.
type PlaybackAck struct {
	PlayedMs    int
	CommittedMs int
	ResponseID  string
	Truncate    bool
}

// Option configures a Provider.
type Option func(*providerConfig)

type providerConfig struct {
	apiKey     string
	endpoint   string
	apiVersion string
	model      string
	voice      string
	dialect    dialect
	beta       bool
	duplex     bool
	heartbeat  time.Duration
	headers    http.Header
	dialer     *websocket.Dialer
}

// WithEndpoint overrides the WebSocket endpoint.
// Use for Azure OpenAI: "wss://<resource>.openai.azure.com/openai/realtime".
func WithEndpoint(rawURL string) Option {
	return func(c *providerConfig) { c.endpoint = rawURL }
}

// WithAPIVersion sets the Azure api-version query parameter.
func WithAPIVersion(v string) Option {
	return func(c *providerConfig) { c.apiVersion = v }
}

// WithAPIKey sets a bearer token. Empty leaves the handshake unauthenticated.
func WithAPIKey(key string) Option {
	return func(c *providerConfig) { c.apiKey = key }
}

// WithModel sets the model sent on the session. Empty omits it.
func WithModel(model string) Option {
	return func(c *providerConfig) { c.model = model }
}

// WithVoice sets the default synthesis voice.
func WithVoice(voice string) Option {
	return func(c *providerConfig) { c.voice = voice }
}

// WithBeta speaks the legacy preview protocol and sends
// OpenAI-Beta: realtime=v1. The default is the GA API.
func WithBeta() Option {
	return func(c *providerConfig) { c.beta = true }
}

// WithDuplex adds ?duplex=1. Duplex deployments already treat a bare
// connection as duplex; set this when the same port also mounts the
// turn-based handler.
func WithDuplex() Option {
	return func(c *providerConfig) { c.duplex = true }
}

// WithHeartbeat sets the vLLM-Omni session.heartbeat interval.
// Zero disables it. OpenAI sessions ignore the heartbeat.
func WithHeartbeat(d time.Duration) Option {
	return func(c *providerConfig) { c.heartbeat = d }
}

// WithHeader sets an extra handshake header.
func WithHeader(key, value string) Option {
	return func(c *providerConfig) {
		if c.headers == nil {
			c.headers = make(http.Header)
		}
		c.headers.Set(key, value)
	}
}

// WithDialer overrides the WebSocket dialer. The default disables
// permessage-deflate and uses 64 KiB buffers.
func WithDialer(d *websocket.Dialer) Option {
	return func(c *providerConfig) { c.dialer = d }
}

// Provider implements niro.RealtimeProvider.
type Provider struct {
	apiKey     string
	endpoint   string
	apiVersion string
	model      string
	voice      string
	dialect    dialect
	beta       bool
	duplex     bool
	heartbeat  time.Duration
	headers    http.Header
	dialer     websocket.Dialer
}

// New creates an OpenAI Realtime provider. apiKey is sent as a Bearer token.
func New(apiKey string, opts ...Option) *Provider {
	return newProvider(apiKey, dialectOpenAI, opts)
}

// NewOmni creates a provider for a vLLM-Omni server.
//
// baseURL accepts http(s) or ws(s). A path of / or empty becomes /v1/realtime.
// The API key may be empty; when set it is sent as a Bearer token.
func NewOmni(baseURL string, opts ...Option) *Provider {
	opts = append([]Option{WithEndpoint(normalizeOmniURL(baseURL)), WithHeartbeat(30 * time.Second)}, opts...)
	return newProvider("", dialectVLLM, opts)
}

func newProvider(apiKey string, d dialect, opts []Option) *Provider {
	pc := &providerConfig{
		apiKey:    apiKey,
		endpoint:  defaultEndpoint,
		dialect:   d,
		heartbeat: 0,
	}
	for _, o := range opts {
		if o != nil {
			o(pc)
		}
	}
	p := &Provider{
		apiKey:     pc.apiKey,
		endpoint:   pc.endpoint,
		apiVersion: pc.apiVersion,
		model:      pc.model,
		voice:      pc.voice,
		dialect:    pc.dialect,
		beta:       pc.beta,
		duplex:     pc.duplex,
		heartbeat:  pc.heartbeat,
		headers:    pc.headers,
	}
	if pc.dialer != nil {
		p.dialer = *pc.dialer
	} else {
		p.dialer = websocket.Dialer{
			Proxy:             http.ProxyFromEnvironment,
			HandshakeTimeout:  handshakeTimeout,
			ReadBufferSize:    64 << 10,
			WriteBufferSize:   64 << 10,
			EnableCompression: false,
			NetDialContext:    (&net.Dialer{Timeout: handshakeTimeout, KeepAlive: 30 * time.Second}).DialContext,
		}
	}
	return p
}

func normalizeOmniURL(base string) string {
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	case strings.HasPrefix(base, "wss://"), strings.HasPrefix(base, "ws://"):
	default:
		base = "ws://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	switch strings.TrimRight(u.Path, "/") {
	case "", "/":
		u.Path = "/v1/realtime"
	case "/v1":
		u.Path = "/v1/realtime"
	}
	return u.String()
}

// Session opens a bidirectional speech session.
// Cancelling ctx closes the session.
func (p *Provider) Session(ctx context.Context, cfg niro.RealtimeConfig) (niro.RealtimeSession, error) {
	model := cfg.Model
	if model == "" {
		model = p.model
	}
	voice := cfg.Voice
	if voice == "" {
		voice = p.voice
	}

	sc := sessionConfigOf(cfg.Extra)
	conn, _, err := p.dialer.DialContext(ctx, p.sessionURL(model, false), p.handshakeHeaders())
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "dial realtime", err)
	}
	recv, emit := niro.NewStream(niro.RealtimeStreamBuffer)

	sess := &Session{
		cfg:              cfg,
		voice:            voice,
		model:            model,
		dialect:          p.dialect,
		beta:             p.beta,
		duplex:           p.duplex,
		heartbeat:        p.heartbeat,
		endpoint:         p.endpoint,
		apiVersion:       p.apiVersion,
		apiKey:           p.apiKey,
		headers:          p.headers,
		dialer:           p.dialer,
		inputRate:        rateFromMIME(cfg.InputFormat, 0),
		outputRate:       rateFromMIME(cfg.OutputFormat, 0),
		explicitResponse: sc != nil && sc.ExplicitResponse,
		provider:         p,
		recv:             recv,
		recvEmit:         emit,
		sendCh:           make(chan outMsg, sendQueue),
		writeLoopDone:    make(chan struct{}),
		closed:           make(chan struct{}),
		funcArgs:         make(map[string]*funcCallAccum, 4),
		funcNames:        make(map[string]string, 4),
		pendingFrames:    make([][]byte, 0, 2),
	}
	sess.connCond = sync.NewCond(&sess.connMu)
	if sc != nil && sc.ModelNative && !sc.ExplicitResponse {
		sess.modelNative.Store(true)
	}
	sess.publishConn(conn)
	fail := func(err error) (niro.RealtimeSession, error) {
		emit.Close()
		conn.Close()
		return nil, err
	}

	update, err := buildSessionUpdate(p, cfg, model, voice, sc)
	if err != nil {
		return fail(err)
	}

	if p.dialect == dialectVLLM {
		if err := sess.writeDirect(ctx, update); err != nil {
			return fail(err)
		}
		if err := sess.readUntil(ctx, "session.created"); err != nil {
			return fail(err)
		}
	} else {
		if err := sess.readUntil(ctx, "session.created"); err != nil {
			return fail(err)
		}
		if err := sess.writeDirect(ctx, update); err != nil {
			return fail(err)
		}
	}
	_ = conn.SetReadDeadline(time.Time{})

	go sess.writeLoop()
	go sess.readLoop()
	go func() {
		select {
		case <-ctx.Done():
			sess.Close()
		case <-sess.closed:
		}
	}()
	return sess, nil
}

func (p *Provider) handshakeHeaders() http.Header {
	h := make(http.Header, len(p.headers)+2)
	for k, v := range p.headers {
		h[k] = append([]string(nil), v...)
	}
	if p.apiKey != "" && h.Get("Authorization") == "" {
		h.Set("Authorization", "Bearer "+p.apiKey)
	}
	if p.beta {
		h.Set("OpenAI-Beta", "realtime=v1")
	}
	return h
}

func (p *Provider) sessionURL(model string, resume bool) string {
	u, err := url.Parse(p.endpoint)
	if err != nil {
		return p.endpoint
	}
	q := u.Query()
	if model != "" && q.Get("model") == "" {
		q.Set("model", model)
	}
	if p.apiVersion != "" && q.Get("api-version") == "" {
		q.Set("api-version", p.apiVersion)
	}
	if p.duplex && q.Get("duplex") == "" {
		q.Set("duplex", "1")
	}
	if resume {
		q.Set("autostart", "0")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func sessionConfigOf(extra any) *SessionConfig {
	switch v := extra.(type) {
	case SessionConfig:
		return &v
	case *SessionConfig:
		return v
	default:
		return nil
	}
}

// Session is a live speech session.
type Session struct {
	cfg              niro.RealtimeConfig
	voice            string
	model            string
	dialect          dialect
	beta             bool
	duplex           bool
	heartbeat        time.Duration
	endpoint         string
	apiVersion       string
	apiKey           string
	headers          http.Header
	dialer           websocket.Dialer
	inputRate        int
	outputRate       int
	explicitResponse bool
	provider         *Provider

	connMu   sync.Mutex
	connCond *sync.Cond
	conn     *websocket.Conn
	connGen  uint64

	recv     *niro.Stream
	recvEmit *niro.Emitter

	sendCh        chan outMsg
	writeLoopDone chan struct{}

	funcArgsMu sync.Mutex
	funcArgs   map[string]*funcCallAccum
	funcNames  map[string]string
	funcSent   map[string]struct{}

	frameMu       sync.Mutex
	pendingFrames [][]byte
	audioPending  atomic.Bool

	sessionID       string
	resumeToken     string
	supportsResume  atomic.Bool
	supportsBargeIn atomic.Bool
	modelNative     atomic.Bool
	lastSeq         atomic.Int64
	lastAck         atomic.Int64
	resumeAttempts  atomic.Int32

	closeOnce sync.Once
	closed    chan struct{}
	err       atomic.Pointer[error]
}

type funcCallAccum struct {
	name string
	args strings.Builder
}

type outMsg struct {
	b  []byte
	bp *[]byte
}

// Send sends a frame to the model.
func (s *Session) Send(ctx context.Context, f niro.Frame) error {
	switch f.Kind {
	case niro.KindAudio:
		return s.sendAudio(ctx, f.Data, f.Mime)
	case niro.KindImage:
		return s.bufferVideo(f.Data)
	case niro.KindText:
		return s.enqueueJSON(ctx, map[string]any{
			"type": "conversation.item.create",
			"item": map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": f.Text},
				},
			},
		})
	case niro.KindToolResult:
		if f.Result == nil {
			return niro.NewError(niro.ErrCodeInvalidRequest, "Send: nil ToolResult")
		}
		return s.sendToolResult(ctx, f.Result)
	case niro.KindControl:
		switch f.Signal {
		case niro.SignalEOT:
			return s.sendEOT(ctx)
		case niro.SignalAbort:
			return s.sendAbort(ctx)
		}
	case niro.KindCustom:
		return s.sendCustom(ctx, f.Custom)
	}
	return nil
}

// Recv returns the model output stream. Call once.
func (s *Session) Recv() *niro.Stream { return s.recv }

// Close terminates the session.
func (s *Session) Close() error {
	var first error
	s.closeOnce.Do(func() {
		close(s.closed)
		s.connMu.Lock()
		c := s.conn
		s.connCond.Broadcast()
		s.connMu.Unlock()
		if c != nil {
			_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(writeWait))
			first = c.Close()
		}
		if s.recvEmit != nil {
			s.recvEmit.Close()
		}
	})
	select {
	case <-s.writeLoopDone:
	case <-time.After(writeWait):
	}
	return first
}

// Err returns the first session error.
func (s *Session) Err() error {
	if ep := s.err.Load(); ep != nil {
		return *ep
	}
	return nil
}

func (s *Session) storeErr(err error) {
	if err != nil {
		s.err.CompareAndSwap(nil, &err)
	}
}

func (s *Session) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

func (s *Session) publishConn(c *websocket.Conn) {
	s.connMu.Lock()
	s.conn = c
	s.connGen++
	s.connCond.Broadcast()
	s.connMu.Unlock()
}

func (s *Session) sendAudio(ctx context.Context, pcm []byte, mime string) error {
	if len(pcm) == 0 {
		return nil
	}
	if mime == "" {
		mime = s.cfg.InputFormat
	}
	rate := rateFromMIME(mime, s.inputRate)
	format := ""
	if mime != "" {
		format = vllmAudioFormat(mime)
	}
	spec := audioWire{rate: rate, format: format, ms: durationMs(pcm, format, rate)}
	var frames [][]byte
	if s.dialect == dialectVLLM {
		s.frameMu.Lock()
		frames = s.pendingFrames
		s.pendingFrames = nil
		s.frameMu.Unlock()
		spec.extended = format != "" || rate > 0 || len(frames) > 0
	}
	bp := getBuf()
	b := encodeAudioAppend((*bp)[:0], pcm, spec, frames)
	*bp = b
	if err := s.enqueue(ctx, outMsg{b: b, bp: bp}); err != nil {
		return err
	}
	s.audioPending.Store(true)
	return nil
}

func (s *Session) bufferVideo(data []byte) error {
	if s.dialect != dialectVLLM || len(data) == 0 {
		return nil
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	s.frameMu.Lock()
	defer s.frameMu.Unlock()
	if len(s.pendingFrames) >= maxVideoFrames {
		copy(s.pendingFrames, s.pendingFrames[1:])
		s.pendingFrames = s.pendingFrames[:maxVideoFrames-1]
	}
	s.pendingFrames = append(s.pendingFrames, cp)
	return nil
}

func (s *Session) sendEOT(ctx context.Context) error {
	if s.audioPending.Swap(false) {
		ev := map[string]any{"type": "input_audio_buffer.commit"}
		if s.dialect == dialectVLLM {
			ev["final"] = true
		}
		if err := s.enqueueJSON(ctx, ev); err != nil {
			return err
		}
	}
	if s.modelNative.Load() {
		return nil
	}
	return s.enqueueJSON(ctx, map[string]any{"type": "response.create"})
}

func (s *Session) sendAbort(ctx context.Context) error {
	if err := s.enqueueJSON(ctx, map[string]any{"type": "response.cancel"}); err != nil {
		return err
	}
	if s.dialect == dialectVLLM {
		return s.enqueueJSON(ctx, map[string]any{"type": "output_audio_buffer.clear"})
	}
	return nil
}

func (s *Session) sendToolResult(ctx context.Context, result *niro.ToolResult) error {
	content := result.Content
	if result.IsError {
		b, _ := json.Marshal(map[string]string{"error": result.Content})
		content = string(b)
	} else if content == "" && len(result.Parts) > 0 {
		b, _ := json.Marshal(map[string]int{"frames": len(result.Parts)})
		content = string(b)
	}
	if err := s.enqueueJSON(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type":    "function_call_output",
			"call_id": result.CallID,
			"output":  content,
		},
	}); err != nil {
		return err
	}
	if err := s.attachCapture(ctx, result.Parts); err != nil {
		return err
	}
	if s.modelNative.Load() {
		return nil
	}
	return s.enqueueJSON(ctx, map[string]any{"type": "response.create"})
}

// attachCapture gives the model the frames a video tool just returned.
// Image frames also wait on the next vLLM audio append as video_frames.
func (s *Session) attachCapture(ctx context.Context, parts []niro.Part) error {
	if len(parts) == 0 {
		return nil
	}
	content := make([]any, 0, len(parts))
	for _, p := range parts {
		ref := mediaRef(p)
		if ref == "" {
			continue
		}
		if imageFrame(p) {
			content = append(content, map[string]any{"type": "input_image", "image_url": ref})
			if err := s.bufferVideo(p.Data); err != nil {
				return err
			}
			continue
		}
		if p.Kind == niro.KindVideo {
			content = append(content, map[string]any{"type": "input_video", "video_url": ref})
		}
	}
	if len(content) == 0 {
		return nil
	}
	return s.enqueueJSON(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type":    "message",
			"role":    "user",
			"content": content,
		},
	})
}

func (s *Session) sendCustom(ctx context.Context, c *niro.ExperimentalFrame) error {
	if c == nil || s.dialect != dialectVLLM {
		return nil
	}
	switch c.Type {
	case EventPlaybackAck:
		ack, ok := playbackAckOf(c.Data)
		if !ok {
			return niro.NewError(niro.ErrCodeInvalidRequest, "playback.ack data must be realtime.PlaybackAck")
		}
		ev := map[string]any{
			"type":      "playback.ack",
			"played_ms": ack.PlayedMs,
		}
		if ack.CommittedMs > 0 {
			ev["committed_ms"] = ack.CommittedMs
		}
		if ack.ResponseID != "" {
			ev["response_id"] = ack.ResponseID
		}
		if ack.Truncate {
			ev["truncate"] = true
		}
		return s.enqueueJSON(ctx, ev)
	case EventBargeIn:
		return s.enqueueJSON(ctx, map[string]any{"type": "barge_in"})
	default:
		return nil
	}
}

func playbackAckOf(data any) (PlaybackAck, bool) {
	switch v := data.(type) {
	case PlaybackAck:
		return v, true
	case *PlaybackAck:
		if v == nil {
			return PlaybackAck{}, false
		}
		return *v, true
	default:
		return PlaybackAck{}, false
	}
}

func (s *Session) enqueueJSON(ctx context.Context, v any) error {
	data, err := niro.JSONMarshal(v)
	if err != nil {
		return niro.WrapError(niro.ErrCodeProviderError, "marshal", err)
	}
	return s.enqueue(ctx, outMsg{b: data})
}

func (s *Session) enqueue(ctx context.Context, msg outMsg) error {
	select {
	case <-s.closed:
		if msg.bp != nil {
			putBuf(msg.bp)
		}
		return niro.ErrClosed
	case <-ctx.Done():
		if msg.bp != nil {
			putBuf(msg.bp)
		}
		return ctx.Err()
	case s.sendCh <- msg:
		return nil
	}
}

func (s *Session) writeDirect(ctx context.Context, payload []byte) error {
	s.connMu.Lock()
	c := s.conn
	s.connMu.Unlock()
	if c == nil {
		return niro.ErrClosed
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetWriteDeadline(dl)
	} else {
		_ = c.SetWriteDeadline(time.Now().Add(writeWait))
	}
	if err := c.WriteMessage(websocket.TextMessage, payload); err != nil {
		return niro.WrapError(niro.ErrCodeProviderError, "write", err)
	}
	return nil
}

func (s *Session) readUntil(ctx context.Context, want string) error {
	s.connMu.Lock()
	c := s.conn
	s.connMu.Unlock()
	if c == nil {
		return niro.ErrClosed
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.SetReadDeadline(time.Now())
		case <-stop:
		}
	}()
	for {
		if dl, ok := ctx.Deadline(); ok {
			_ = c.SetReadDeadline(dl)
		} else {
			_ = c.SetReadDeadline(time.Now().Add(handshakeTimeout))
		}
		_, msg, err := c.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return niro.WrapError(niro.ErrCodeStreamError, "waiting for "+want, err)
		}
		top := scanTop(msg)
		if eq(top.typ, want) {
			var ev serverEvent
			if niro.JSONUnmarshal(msg, &ev) == nil {
				s.absorbSession(ev)
			}
			return nil
		}
		if eq(top.typ, "error") {
			var ev serverEvent
			_ = niro.JSONUnmarshal(msg, &ev)
			msgText, code := errorText(ev.Error)
			return niro.NewErrorf(niro.ErrCodeProviderError, "server error [%s]: %s", code, msgText)
		}
	}
}

func (s *Session) writeLoop() {
	defer close(s.writeLoopDone)
	var tick <-chan time.Time
	if s.dialect == dialectVLLM && s.heartbeat > 0 {
		t := time.NewTicker(s.heartbeat)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-s.closed:
			return
		case <-tick:
			_ = s.writeAll([]byte(`{"type":"session.heartbeat"}`))
		case msg := <-s.sendCh:
			err := s.writeAll(msg.b)
			if msg.bp != nil {
				putBuf(msg.bp)
			}
			if err != nil {
				if !s.isClosed() {
					s.storeErr(err)
				}
				return
			}
		}
	}
}

func (s *Session) writeAll(b []byte) error {
	for attempt := 0; attempt < 8; attempt++ {
		if s.isClosed() {
			return niro.ErrClosed
		}
		s.connMu.Lock()
		for s.conn == nil && !s.isClosed() {
			s.connCond.Wait()
		}
		c := s.conn
		gen := s.connGen
		closed := s.isClosed()
		s.connMu.Unlock()
		if closed || c == nil {
			return niro.ErrClosed
		}
		_ = c.SetWriteDeadline(time.Now().Add(writeWait))
		err := c.WriteMessage(websocket.TextMessage, b)
		if err == nil {
			return nil
		}
		if s.isClosed() {
			return err
		}
		s.connMu.Lock()
		for s.connGen == gen && !s.isClosed() {
			s.connCond.Wait()
		}
		stalled := s.connGen == gen || s.isClosed()
		s.connMu.Unlock()
		if stalled {
			return err
		}
	}
	return niro.NewError(niro.ErrCodeProviderError, "realtime write failed after resume")
}

func (s *Session) readLoop() {
	defer s.recvEmit.Close()
	for {
		if s.isClosed() {
			return
		}
		s.connMu.Lock()
		for s.conn == nil && !s.isClosed() {
			s.connCond.Wait()
		}
		c := s.conn
		closed := s.isClosed()
		s.connMu.Unlock()
		if closed || c == nil {
			return
		}
		_, msg, err := c.ReadMessage()
		if err != nil {
			if s.isClosed() {
				return
			}
			if s.resumeAttempts.Add(1) <= maxResume && s.tryResume() {
				continue
			}
			s.storeErr(err)
			s.recvEmit.Error(err)
			s.Close()
			return
		}
		s.resumeAttempts.Store(0)
		if !s.handleMessage(msg) {
			s.Close()
			return
		}
	}
}

func (s *Session) tryResume() bool {
	if s.dialect != dialectVLLM || !s.supportsResume.Load() || s.sessionID == "" || s.resumeToken == "" {
		return false
	}
	s.connMu.Lock()
	old := s.conn
	s.conn = nil
	s.connMu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	conn, _, err := s.dialer.DialContext(ctx, s.provider.sessionURL(s.model, true), s.provider.handshakeHeaders())
	if err != nil {
		return false
	}
	body, err := niro.JSONMarshal(map[string]any{
		"type":                           "session.resume",
		"session_id":                     s.sessionID,
		"resume_token":                   s.resumeToken,
		"last_received_server_event_seq": s.lastSeq.Load(),
	})
	if err != nil {
		conn.Close()
		return false
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := conn.WriteMessage(websocket.TextMessage, body); err != nil {
		conn.Close()
		return false
	}
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			conn.Close()
			return false
		}
		top := scanTop(msg)
		if eq(top.typ, "session.resumed") || eq(top.typ, "session.created") {
			var ev serverEvent
			if niro.JSONUnmarshal(msg, &ev) == nil {
				s.absorbSession(ev)
			}
			_ = conn.SetReadDeadline(time.Time{})
			if s.isClosed() {
				conn.Close()
				return false
			}
			s.publishConn(conn)
			return true
		}
		if eq(top.typ, "error") || eq(top.typ, "session.expired") || eq(top.typ, "session.resync_required") {
			conn.Close()
			return false
		}
	}
}

func buildSessionUpdate(p *Provider, cfg niro.RealtimeConfig, model, voice string, sc *SessionConfig) ([]byte, error) {
	session := map[string]any{}
	if cfg.SystemPrompt != "" {
		session["instructions"] = cfg.SystemPrompt
	}
	if len(cfg.Tools) > 0 {
		tools := make([]map[string]any, len(cfg.Tools))
		for i, t := range cfg.Tools {
			tool := map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
			}
			if len(t.Parameters) > 0 {
				tool["parameters"] = t.Parameters
			}
			tools[i] = tool
		}
		session["tools"] = tools
	}
	if choice := toolChoiceValue(cfg.ToolChoice); choice != nil {
		session["tool_choice"] = choice
	}
	if cfg.Options.Temperature != nil {
		session["temperature"] = *cfg.Options.Temperature
	}

	inMIME := cfg.InputFormat
	outMIME := cfg.OutputFormat
	td, includeTD := turnDetection(cfg, p.dialect)
	mods := outputModalities(sc)

	switch {
	case p.dialect == dialectVLLM:
		if model != "" {
			session["model"] = model
		}
		if len(mods) == 0 {
			mods = []string{"audio", "text"}
		}
		session["modalities"] = mods
		applyFlatAudio(session, inMIME, outMIME, voice)
		if audio := nestedAudio(inMIME, outMIME, voice); audio != nil {
			session["audio"] = audio
		}
		if includeTD {
			session["turn_detection"] = td
		}
		if cfg.Options.MaxTokens > 0 {
			session["max_response_output_tokens"] = cfg.Options.MaxTokens
		}
		extra := map[string]any{}
		if sc != nil {
			if sc.RefAudio != "" {
				session["ref_audio"] = sc.RefAudio
			}
			if sc.OverlapPolicy != "" {
				session["overlap_policy"] = sc.OverlapPolicy
			}
			if sc.PlaybackCommitPolicy != "" {
				session["playback_commit_policy"] = sc.PlaybackCommitPolicy
			}
			if sc.IdleTimeoutS > 0 {
				session["idle_timeout_s"] = sc.IdleTimeoutS
			}
			if sc.AutoResponse != nil {
				extra["auto_response"] = *sc.AutoResponse
			} else if sc.ModelNative {
				extra["auto_response"] = false
			}
			if sc.ForceListen != nil {
				extra["force_listen_count"] = *sc.ForceListen
			}
			for k, v := range sc.ExtraBody {
				extra[k] = v
			}
			if sc.Speed != nil {
				session["speed"] = *sc.Speed
			}
		}
		if len(extra) > 0 {
			session["extra_body"] = extra
		}
	case p.beta:
		if len(mods) > 0 {
			session["modalities"] = mods
		}
		applyFlatAudio(session, inMIME, outMIME, voice)
		if includeTD {
			session["turn_detection"] = td
		}
		if cfg.Options.MaxTokens > 0 {
			session["max_response_output_tokens"] = cfg.Options.MaxTokens
		}
	default:
		session["type"] = "realtime"
		if model != "" {
			session["model"] = model
		}
		if len(mods) > 0 {
			session["output_modalities"] = mods
		}
		in := map[string]any{}
		if format := formatObject(inMIME); format != nil {
			in["format"] = format
		}
		if includeTD {
			in["turn_detection"] = td
		}
		if sc != nil && sc.TranscriptionModel != "" {
			in["transcription"] = map[string]any{"model": sc.TranscriptionModel}
		}
		if sc != nil && sc.NoiseReduction != "" {
			in["noise_reduction"] = map[string]any{"type": sc.NoiseReduction}
		}
		out := map[string]any{}
		if format := formatObject(outMIME); format != nil {
			out["format"] = format
		}
		if voice != "" {
			out["voice"] = voice
		}
		if sc != nil && sc.Speed != nil {
			out["speed"] = *sc.Speed
		}
		if len(in) > 0 || len(out) > 0 {
			session["audio"] = map[string]any{"input": in, "output": out}
		}
		if cfg.Options.MaxTokens > 0 {
			session["max_output_tokens"] = cfg.Options.MaxTokens
		}
	}

	return niro.JSONMarshal(map[string]any{
		"type":    "session.update",
		"session": session,
	})
}

func outputModalities(sc *SessionConfig) []string {
	if sc == nil {
		return nil
	}
	return sc.OutputModalities
}

func applyFlatAudio(session map[string]any, inMIME, outMIME, voice string) {
	if inMIME != "" {
		session["input_audio_format"] = vllmAudioFormat(inMIME)
	}
	if outMIME != "" {
		session["output_audio_format"] = vllmAudioFormat(outMIME)
	}
	if voice != "" {
		session["voice"] = voice
	}
}

func nestedAudio(inMIME, outMIME, voice string) map[string]any {
	in := map[string]any{}
	if format := formatObject(inMIME); format != nil {
		in["format"] = format
		if rate := rateFromMIME(inMIME, 0); rate > 0 {
			in["sample_rate_hz"] = rate
		}
	}
	out := map[string]any{}
	if format := formatObject(outMIME); format != nil {
		out["format"] = format
	}
	if voice != "" {
		out["voice"] = voice
	}
	if len(in) == 0 && len(out) == 0 {
		return nil
	}
	audio := map[string]any{}
	if len(in) > 0 {
		audio["input"] = in
	}
	if len(out) > 0 {
		audio["output"] = out
	}
	return audio
}

func formatObject(mime string) map[string]any {
	if mime == "" {
		return nil
	}
	obj := map[string]any{"type": oaiAudioType(mime)}
	if rate := rateFromMIME(mime, 0); rate > 0 {
		obj["rate"] = rate
	}
	return obj
}

// turnDetection returns the turn_detection value and whether the field is sent.
// "omit" leaves the key out so a duplex server keeps its own default.
// "manual" and an empty mode with no VAD send JSON null (caller commits turns).
func turnDetection(cfg niro.RealtimeConfig, d dialect) (any, bool) {
	if cfg.TurnDetection == "omit" {
		return nil, false
	}
	if cfg.TurnDetection == "manual" || (cfg.TurnDetection == "" && cfg.VAD == nil) {
		return nil, true
	}
	mode := cfg.TurnDetection
	if cfg.VAD != nil && cfg.VAD.Mode != "" {
		mode = cfg.VAD.Mode
	}
	if mode == "" || (d == dialectVLLM && mode == "semantic_vad") {
		mode = "server_vad"
	}
	vad := cfg.VAD
	if vad == nil {
		vad = &niro.VADConfig{}
	}
	td := map[string]any{"type": mode}
	if mode == "semantic_vad" {
		if vad.Eagerness != "" {
			td["eagerness"] = vad.Eagerness
		}
	} else {
		if vad.Threshold > 0 {
			td["threshold"] = vad.Threshold
		}
		if vad.PrefixPaddingMs > 0 {
			td["prefix_padding_ms"] = vad.PrefixPaddingMs
		}
		if vad.SilenceDurationMs > 0 {
			td["silence_duration_ms"] = vad.SilenceDurationMs
		}
	}
	if d != dialectVLLM {
		create := true
		if vad.CreateResponse != nil {
			create = *vad.CreateResponse
		}
		interrupt := true
		if vad.InterruptResponse != nil {
			interrupt = *vad.InterruptResponse
		}
		td["create_response"] = create
		td["interrupt_response"] = interrupt
	}
	return td, true
}
