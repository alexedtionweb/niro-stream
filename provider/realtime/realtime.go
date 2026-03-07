// Package realtime implements a Niro RealtimeProvider backed by the
// OpenAI Realtime API (wss://api.openai.com/v1/realtime).
//
// The Realtime API uses a persistent WebSocket connection for bidirectional
// audio streaming. Both input and output audio are 24 kHz 16-bit mono PCM
// encoded as base64.
//
// Usage:
//
//	p := realtime.New(os.Getenv("OPENAI_API_KEY"))
//	sess, err := p.Session(ctx, niro.RealtimeConfig{
//	    Model:        "gpt-4o-realtime-preview",
//	    SystemPrompt: "You are a helpful voice assistant.",
//	    Voice:        "alloy",
//	    VAD: &niro.VADConfig{
//	        Threshold:         0.5,
//	        PrefixPaddingMs:   300,
//	        SilenceDurationMs: 200,
//	    },
//	})
//	defer sess.Close()
//
// Audio in/out: 24 kHz PCM16 mono (niro.AudioPCM24k).
//
// Provider limitations (as of 2025-02):
//   - No per-chunk transcription; transcript arrives after the full turn.
//   - function_call arguments are streamed; KindToolCall emitted on .done.
//   - Custom output audio format is not yet configurable via this SDK.
//   - Azure OpenAI Realtime uses a slightly different endpoint and auth;
//     use WithEndpoint / WithAPIVersion for Azure deployments.
package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/alexedtionweb/niro-stream"
)

const (
	defaultEndpoint  = "wss://api.openai.com/v1/realtime"
	defaultModel     = "gpt-4o-realtime-preview"
	defaultVoice     = "alloy"
	writeWait        = 10 * time.Second
	handshakeTimeout = 15 * time.Second
)

// Option configures a Provider.
type Option func(*providerConfig)

type providerConfig struct {
	endpoint   string
	apiVersion string // for Azure OpenAI
	model      string
	voice      string
}

// WithEndpoint overrides the WebSocket endpoint.
// Use for Azure OpenAI: "wss://<deployment>.openai.azure.com/openai/realtime"
func WithEndpoint(url string) Option {
	return func(c *providerConfig) { c.endpoint = url }
}

// WithAPIVersion sets the Azure api-version query parameter.
// Only needed for Azure OpenAI endpoints.
func WithAPIVersion(v string) Option {
	return func(c *providerConfig) { c.apiVersion = v }
}

// WithModel sets the default model.
func WithModel(model string) Option {
	return func(c *providerConfig) { c.model = model }
}

// WithVoice sets the default synthesis voice.
func WithVoice(voice string) Option {
	return func(c *providerConfig) { c.voice = voice }
}

// Provider implements niro.RealtimeProvider for the OpenAI Realtime API.
type Provider struct {
	apiKey     string
	endpoint   string
	apiVersion string
	model      string
	voice      string
}

// New creates a Provider with the given OpenAI API key.
func New(apiKey string, opts ...Option) *Provider {
	pc := &providerConfig{
		endpoint: defaultEndpoint,
		model:    defaultModel,
		voice:    defaultVoice,
	}
	for _, o := range opts {
		o(pc)
	}
	return &Provider{
		apiKey:     apiKey,
		endpoint:   pc.endpoint,
		apiVersion: pc.apiVersion,
		model:      pc.model,
		voice:      pc.voice,
	}
}

// Session opens a new Realtime session.
//
// The method blocks until the WebSocket handshake completes and the server
// sends session.created. The caller owns the returned session and must call
// Close when done.
func (p *Provider) Session(ctx context.Context, cfg niro.RealtimeConfig) (niro.RealtimeSession, error) {
	model := cfg.Model
	if model == "" {
		model = p.model
	}
	voice := cfg.Voice
	if voice == "" {
		voice = p.voice
	}
	if cfg.InputFormat == "" {
		cfg.InputFormat = niro.AudioPCM24k
	}
	if cfg.OutputFormat == "" {
		cfg.OutputFormat = niro.AudioPCM24k
	}

	url := p.endpoint + "?model=" + model
	if p.apiVersion != "" {
		url += "&api-version=" + p.apiVersion
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+p.apiKey)
	headers.Set("OpenAI-Beta", "realtime=v1")

	dialer := websocket.Dialer{HandshakeTimeout: handshakeTimeout}
	conn, _, err := dialer.DialContext(ctx, url, headers)
	if err != nil {
		return nil, fmt.Errorf("realtime: dial %s: %w", url, err)
	}

	recv, recvEmit := niro.NewStream(niro.RealtimeStreamBuffer)
	sess := &Session{
		cfg:           cfg,
		voice:         voice,
		conn:          conn,
		recv:          recv,
		recvEmit:      recvEmit,
		sendCh:        make(chan []byte, 32),
		writeLoopDone: make(chan struct{}),
		closed:        make(chan struct{}),
		funcArgs:      make(map[string]*funcCallAccum, 4),
	}

	// Wait for session.created before proceeding.
	if err := sess.waitSessionCreated(ctx); err != nil {
		conn.Close()
		return nil, err
	}

	// Configure the session.
	if err := sess.configure(cfg, voice); err != nil {
		conn.Close()
		return nil, err
	}

	// Start background goroutines.
	go sess.writeLoop()
	go sess.readLoop()

	return sess, nil
}

// ────────────────────────────────────────────────────────────────────────────
// Session
// ────────────────────────────────────────────────────────────────────────────

// Session is a live OpenAI Realtime speech session.
type Session struct {
	cfg   niro.RealtimeConfig
	voice string
	conn  *websocket.Conn

	recv     *niro.Stream
	recvEmit *niro.Emitter

	// sendCh serialises WebSocket writes — gorilla/websocket forbids concurrent writes.
	sendCh        chan []byte
	writeLoopDone chan struct{} // closed by writeLoop when it exits

	// funcArgs accumulates streamed function-call arguments keyed by call_id.
	// Pre-allocated in Session() to avoid lazy-init cost on the hot receive path.
	funcArgsMu sync.Mutex
	funcArgs   map[string]*funcCallAccum

	closeOnce sync.Once
	closed    chan struct{}
	err       atomic.Pointer[error]
}

type funcCallAccum struct {
	name string
	args strings.Builder
}

// ── Public API ───────────────────────────────────────────────────────────────

// Send sends a frame to the model.
func (s *Session) Send(ctx context.Context, f niro.Frame) error {
	switch f.Kind {
	case niro.KindAudio:
		return s.sendAudio(f.Data)
	case niro.KindText:
		return s.sendTextMessage(f.Text)
	case niro.KindToolResult:
		if f.Result == nil {
			return errors.New("realtime: Send: nil ToolResult")
		}
		return s.sendToolResult(f.Result)
	case niro.KindControl:
		switch f.Signal {
		case niro.SignalEOT:
			return s.sendEOT()
		case niro.SignalAbort:
			return s.sendAbort()
		}
	}
	return nil
}

// Recv returns the read-only model output stream.
func (s *Session) Recv() *niro.Stream { return s.recv }

// Close terminates the session gracefully.
//
// gorilla/websocket forbids concurrent writes, so Close must not call
// WriteControl while writeLoop may be in WriteMessage. Instead we signal
// s.closed so writeLoop sends the close frame itself, then wait for writeLoop
// to finish before closing the connection.
func (s *Session) Close() error {
	var firstErr error
	s.closeOnce.Do(func() {
		close(s.closed) // signals writeLoop to send close frame and exit
		// Wait for writeLoop to flush the close frame (or time out).
		select {
		case <-s.writeLoopDone:
		case <-time.After(writeWait):
		}
		firstErr = s.conn.Close() // unblocks readLoop's ReadMessage
		s.recvEmit.Close()
	})
	return firstErr
}

// Err returns the first session error.
func (s *Session) Err() error {
	if ep := s.err.Load(); ep != nil {
		return *ep
	}
	return nil
}

// ── Send helpers ─────────────────────────────────────────────────────────────

// rtAudioPool pools []byte encoding buffers for the sendAudio hot path.
// At 50 ms chunks, 24 kHz PCM16 mono = 2400 bytes → ~3200 bytes base64.
var rtAudioPool = sync.Pool{New: func() any {
	b := make([]byte, 0, 4096)
	return &b
}}

func (s *Session) sendAudio(data []byte) error {
	bp := rtAudioPool.Get().(*[]byte)
	b := (*bp)[:0]
	b = append(b, `{"type":"input_audio_buffer.append","audio":"`...)
	encLen := base64.StdEncoding.EncodedLen(len(data))
	if need := len(b) + encLen + 2; cap(b) < need {
		grown := make([]byte, len(b), need+256)
		copy(grown, b)
		b = grown
	}
	start := len(b)
	b = b[:start+encLen]
	base64.StdEncoding.Encode(b[start:], data)
	b = append(b, '"', '}')
	payload := make([]byte, len(b))
	copy(payload, b)
	*bp = b[:0]
	rtAudioPool.Put(bp)
	select {
	case <-s.closed:
		return niro.ErrClosed
	case s.sendCh <- payload:
		return nil
	}
}

func (s *Session) sendTextMessage(text string) error {
	return s.enqueue(rtClientEvent{
		Type: "conversation.item.create",
		Item: &rtConversationItem{
			Type: "message",
			Role: "user",
			Content: []rtContentPart{
				{Type: "input_text", Text: text},
			},
		},
	})
}

// sendEOT commits the audio buffer and requests a response.
func (s *Session) sendEOT() error {
	if err := s.enqueue(rtClientEvent{Type: "input_audio_buffer.commit"}); err != nil {
		return err
	}
	return s.enqueue(rtClientEvent{Type: "response.create"})
}

// sendAbort cancels an in-progress response (barge-in).
func (s *Session) sendAbort() error {
	return s.enqueue(rtClientEvent{Type: "response.cancel"})
}

// sendToolResult delivers a function call result and requests the next response.
func (s *Session) sendToolResult(result *niro.ToolResult) error {
	content := result.Content
	if result.IsError {
		content = `{"error":"` + jsonEscape(result.Content) + `"}`
	}
	if err := s.enqueue(rtClientEvent{
		Type: "conversation.item.create",
		Item: &rtConversationItem{
			Type:   "function_call_output",
			CallID: result.CallID,
			Output: content,
		},
	}); err != nil {
		return err
	}
	return s.enqueue(rtClientEvent{Type: "response.create"})
}

// enqueue serialises an event and puts it on the write channel.
func (s *Session) enqueue(ev rtClientEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("realtime: marshal: %w", err)
	}
	select {
	case <-s.closed:
		return niro.ErrClosed
	case s.sendCh <- data:
		return nil
	}
}

// ── Session initialisation ────────────────────────────────────────────────────

func (s *Session) waitSessionCreated(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		for {
			_, msg, err := s.conn.ReadMessage()
			if err != nil {
				done <- fmt.Errorf("realtime: waiting for session.created: %w", err)
				return
			}
			var ev rtServerEvent
			if json.Unmarshal(msg, &ev) == nil && ev.Type == "session.created" {
				done <- nil
				return
			}
			if ev.Type == "error" {
				done <- fmt.Errorf("realtime: server error on connect: %s", ev.Error.Message)
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		// Set an immediate read deadline so the goroutine's ReadMessage
		// returns with a timeout error and the goroutine exits promptly.
		// The caller will close the conn, which also unblocks any pending read.
		_ = s.conn.SetReadDeadline(time.Now())
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func (s *Session) configure(cfg niro.RealtimeConfig, voice string) error {
	upd := rtSessionUpdate{
		Instructions: cfg.SystemPrompt,
		Voice:        voice,
		ToolChoice:   string(cfg.ToolChoice),
	}

	if len(cfg.Tools) > 0 {
		upd.Tools = make([]rtTool, len(cfg.Tools))
		for i, t := range cfg.Tools {
			upd.Tools[i] = rtTool{
				Type:        "function",
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			}
		}
	}

	if cfg.VAD != nil {
		upd.TurnDetection = &rtTurnDetection{
			Type:              "server_vad",
			Threshold:         cfg.VAD.Threshold,
			PrefixPaddingMs:   cfg.VAD.PrefixPaddingMs,
			SilenceDurationMs: cfg.VAD.SilenceDurationMs,
		}
	} else {
		// Disable server-side VAD; caller controls turn boundaries.
		upd.TurnDetection = &rtTurnDetection{Type: "none"}
	}

	if cfg.Options.MaxTokens > 0 {
		upd.MaxResponseOutputTokens = cfg.Options.MaxTokens
	}

	data, err := json.Marshal(rtClientEvent{
		Type:    "session.update",
		Session: &upd,
	})
	if err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.TextMessage, data)
}

// ── Background loops ──────────────────────────────────────────────────────────

// writeLoop serialises all WebSocket writes; gorilla/websocket forbids concurrent writes.
// On shutdown (s.closed) it sends a graceful WebSocket close frame then signals writeLoopDone.
func (s *Session) writeLoop() {
	defer close(s.writeLoopDone)
	for {
		select {
		case <-s.closed:
			// We are the sole writer — safe to send the close frame here.
			msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
			_ = s.conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(writeWait))
			return
		case data := <-s.sendCh:
			_ = s.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := s.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				s.storeErr(err)
				return
			}
		}
	}
}

// readLoop reads server events and emits Frames to the receive stream.
func (s *Session) readLoop() {
	defer s.recvEmit.Close()

	for {
		select {
		case <-s.closed:
			return
		default:
		}

		_, msg, err := s.conn.ReadMessage()
		if err != nil {
			select {
			case <-s.closed:
				return // normal shutdown
			default:
				s.storeErr(err)
				s.recvEmit.Error(err)
				return
			}
		}

		var ev rtServerEvent
		if json.Unmarshal(msg, &ev) != nil {
			continue
		}

		s.handleServerEvent(ev)
	}
}

func (s *Session) handleServerEvent(ev rtServerEvent) {
	ctx := context.Background()

	switch ev.Type {
	// ── Audio output ───────────────────────────────────────────────────────
	case "response.output_audio.delta":
		if ev.Delta == "" {
			return
		}
		pcm, err := base64.StdEncoding.DecodeString(ev.Delta)
		if err != nil || len(pcm) == 0 {
			return
		}
		_ = s.recvEmit.Emit(ctx, niro.AudioFrame(pcm, niro.AudioPCM24k))

	// ── Text / transcript output ───────────────────────────────────────────
	case "response.output_audio_transcript.delta":
		if ev.Delta != "" {
			_ = s.recvEmit.Emit(ctx, niro.TextFrame(ev.Delta))
		}

	case "response.output_text.delta":
		if ev.Delta != "" {
			_ = s.recvEmit.Emit(ctx, niro.TextFrame(ev.Delta))
		}

	// ── Function / tool calls ─────────────────────────────────────────────
	case "response.function_call_arguments.delta":
		if ev.CallID == "" {
			return
		}
		s.funcArgsMu.Lock()
		acc := s.funcArgs[ev.CallID]
		if acc == nil {
			acc = &funcCallAccum{}
			s.funcArgs[ev.CallID] = acc
		}
		acc.args.WriteString(ev.Delta)
		s.funcArgsMu.Unlock()

	case "response.function_call_arguments.done":
		if ev.CallID == "" {
			return
		}
		s.funcArgsMu.Lock()
		acc := s.funcArgs[ev.CallID]
		if acc != nil {
			delete(s.funcArgs, ev.CallID)
		}
		s.funcArgsMu.Unlock()

		args := ev.Arguments // server also provides full args here
		if args == "" && acc != nil {
			args = acc.args.String()
		}
		argsJSON := json.RawMessage(args)
		if !json.Valid(argsJSON) {
			argsJSON = []byte(`{}`)
		}
		name := ev.Name
		if name == "" && acc != nil {
			name = acc.name
		}
		_ = s.recvEmit.Emit(ctx, niro.ToolCallFrame(&niro.ToolCall{
			ID:   ev.CallID,
			Name: name,
			Args: argsJSON,
		}))

	// ── Barge-in / VAD ────────────────────────────────────────────────────
	case "input_audio_buffer.speech_started":
		// User started speaking while model is generating — signal barge-in.
		_ = s.recvEmit.Emit(ctx, niro.ControlFrame(niro.SignalFlush))

	// ── Turn end ──────────────────────────────────────────────────────────
	case "response.done":
		// Emit usage if present.
		if ev.Response != nil && ev.Response.Usage != nil {
			u := &niro.Usage{
				InputTokens:  ev.Response.Usage.InputTokens,
				OutputTokens: ev.Response.Usage.OutputTokens,
				TotalTokens:  ev.Response.Usage.TotalTokens,
				Detail:       map[string]int{},
			}
			d := ev.Response.Usage.InputTokenDetails
			if d != nil {
				u.Detail["input_audio_tokens"] = d.AudioTokens
				u.Detail["input_text_tokens"] = d.TextTokens
				u.Detail["input_cached_tokens"] = d.CachedTokens
			}
			od := ev.Response.Usage.OutputTokenDetails
			if od != nil {
				u.Detail["output_audio_tokens"] = od.AudioTokens
				u.Detail["output_text_tokens"] = od.TextTokens
			}
			_ = s.recvEmit.Emit(ctx, niro.UsageFrame(u))
		}
		_ = s.recvEmit.Emit(ctx, niro.ControlFrame(niro.SignalEOT))

	// ── Errors ────────────────────────────────────────────────────────────
	case "error":
		err := fmt.Errorf("realtime: server error [%s]: %s", ev.Error.Code, ev.Error.Message)
		s.storeErr(err)
		s.recvEmit.Error(err)
	}
}

func (s *Session) storeErr(err error) {
	s.err.CompareAndSwap(nil, &err)
}

// ── JSON event types — client → server ───────────────────────────────────────

type rtClientEvent struct {
	Type    string              `json:"type"`
	Audio   string              `json:"audio,omitempty"`   // input_audio_buffer.append
	Item    *rtConversationItem `json:"item,omitempty"`    // conversation.item.create
	Session *rtSessionUpdate    `json:"session,omitempty"` // session.update
}

type rtConversationItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role,omitempty"`
	Content []rtContentPart `json:"content,omitempty"`
	CallID  string          `json:"call_id,omitempty"`
	Output  string          `json:"output,omitempty"`
}

type rtContentPart struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Audio string `json:"audio,omitempty"` // base64
}

type rtSessionUpdate struct {
	Instructions            string           `json:"instructions,omitempty"`
	Voice                   string           `json:"voice,omitempty"`
	Tools                   []rtTool         `json:"tools,omitempty"`
	ToolChoice              string           `json:"tool_choice,omitempty"`
	TurnDetection           *rtTurnDetection `json:"turn_detection,omitempty"`
	MaxResponseOutputTokens int              `json:"max_response_output_tokens,omitempty"`
}

type rtTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type rtTurnDetection struct {
	Type              string  `json:"type"`
	Threshold         float64 `json:"threshold,omitempty"`
	PrefixPaddingMs   int     `json:"prefix_padding_ms,omitempty"`
	SilenceDurationMs int     `json:"silence_duration_ms,omitempty"`
}

// ── JSON event types — server → client ───────────────────────────────────────

type rtServerEvent struct {
	Type      string      `json:"type"`
	EventID   string      `json:"event_id,omitempty"`
	Delta     string      `json:"delta,omitempty"`     // audio delta (base64) or text delta
	CallID    string      `json:"call_id,omitempty"`   // function call
	Name      string      `json:"name,omitempty"`      // function name (on .done)
	Arguments string      `json:"arguments,omitempty"` // full args (on .done)
	Response  *rtResponse `json:"response,omitempty"`
	Error     rtError     `json:"error"`
}

type rtResponse struct {
	Status string   `json:"status"`
	Usage  *rtUsage `json:"usage,omitempty"`
}

type rtUsage struct {
	TotalTokens        int             `json:"total_tokens"`
	InputTokens        int             `json:"input_tokens"`
	OutputTokens       int             `json:"output_tokens"`
	InputTokenDetails  *rtTokenDetails `json:"input_token_details,omitempty"`
	OutputTokenDetails *rtTokenDetails `json:"output_token_details,omitempty"`
}

type rtTokenDetails struct {
	AudioTokens  int `json:"audio_tokens"`
	TextTokens   int `json:"text_tokens"`
	CachedTokens int `json:"cached_tokens"`
}

type rtError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ── Utilities ─────────────────────────────────────────────────────────────────

// jsonEscape minimally escapes a string for embedding in a JSON string literal.
func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	// json.Marshal produces `"..."` — strip the outer quotes.
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}
