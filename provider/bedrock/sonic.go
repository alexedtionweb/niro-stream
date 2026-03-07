package bedrock

// Nova Sonic — Amazon's speech-to-speech foundation model accessed via the
// Bedrock Runtime bidirectional streaming API (InvokeModelWithBidirectionalStream).
//
// Sonic implements niro.RealtimeProvider. Obtain a session and stream microphone
// audio in while playing model audio out:
//
//	cfg, _ := config.LoadDefaultConfig(ctx)
//	sonic := bedrock.NewSonic(cfg)
//	sess, err := sonic.Session(ctx, niro.RealtimeConfig{
//	    SystemPrompt: "You are a helpful voice assistant.",
//	    Voice:        "matthew",
//	    Tools:        myTools,
//	})
//	defer sess.Close()
//
// Audio in:  16 kHz PCM16 mono (niro.AudioPCM16k)
// Audio out: 24 kHz PCM16 mono (niro.AudioPCM24k)
//
// Provider limitations (as of Nova Sonic v1):
//   - Session max duration: 8 minutes.
//   - No streaming function arguments: tool calls are delivered complete.
//   - speculative (low-latency preview) text is filtered; only FINAL text emitted.
//   - Input sample rate must be exactly 16 000 Hz.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/google/uuid"

	"github.com/alexedtionweb/niro-stream"
)

// ── Audio event encoding pool ─────────────────────────────────────────────────
//
// Every audio chunk follows the same JSON template:
//
//	{"event":{"audioInput":{"promptName":"<36>","contentName":"<36>","content":"<base64>"}}}
//
// A pool of reusable []byte avoids reflect-based json.Marshal and a string alloc
// on every frame — typically 50–100 frames/second per active session.
var sonicAudioPool = sync.Pool{New: func() any {
	b := make([]byte, 0, 4096)
	return &b
}}

// sonicEncodeAudioEvent serialises an audioInput event without reflection.
// Returns a freshly-allocated []byte owned by the caller.
func sonicEncodeAudioEvent(promptID, contentID string, raw []byte) []byte {
	bp := sonicAudioPool.Get().(*[]byte)
	b := (*bp)[:0]

	b = append(b, `{"event":{"audioInput":{"promptName":"`...)
	b = append(b, promptID...)
	b = append(b, `","contentName":"`...)
	b = append(b, contentID...)
	b = append(b, `","content":"`...)

	encLen := base64.StdEncoding.EncodedLen(len(raw))
	if need := len(b) + encLen + 5; cap(b) < need {
		grown := make([]byte, len(b), need+256)
		copy(grown, b)
		b = grown
	}
	start := len(b)
	b = b[:start+encLen]
	base64.StdEncoding.Encode(b[start:], raw)
	b = append(b, '"', '}', '}', '}')

	// Copy before returning to pool (AWS SDK reads Bytes synchronously inside
	// Writer.Send, but we are conservative about the contract).
	out := make([]byte, len(b))
	copy(out, b)
	*bp = b[:0]
	sonicAudioPool.Put(bp)
	return out
}

// SonicOption configures a SonicProvider.
type SonicOption func(*sonicConfig)

type sonicConfig struct {
	model string
	voice string
}

// WithSonicModel sets the default Nova Sonic model ID.
func WithSonicModel(model string) SonicOption {
	return func(c *sonicConfig) { c.model = model }
}

// WithSonicVoice sets the default synthesis voice (e.g. "matthew", "tiffany").
func WithSonicVoice(voice string) SonicOption {
	return func(c *sonicConfig) { c.voice = voice }
}

// SonicProvider implements niro.RealtimeProvider backed by Amazon Nova Sonic.
type SonicProvider struct {
	client *bedrockruntime.Client
	model  string
	voice  string
}

// NewSonic creates a SonicProvider from an AWS config.
func NewSonic(cfg aws.Config, opts ...SonicOption) *SonicProvider {
	sc := &sonicConfig{model: "amazon.nova-sonic-v1:0", voice: "matthew"}
	for _, o := range opts {
		o(sc)
	}
	return &SonicProvider{
		client: bedrockruntime.NewFromConfig(cfg),
		model:  sc.model,
		voice:  sc.voice,
	}
}

// NewSonicFromClient creates a SonicProvider from an existing Bedrock client.
func NewSonicFromClient(client *bedrockruntime.Client, opts ...SonicOption) *SonicProvider {
	sc := &sonicConfig{model: "amazon.nova-sonic-v1:0", voice: "matthew"}
	for _, o := range opts {
		o(sc)
	}
	return &SonicProvider{client: client, model: sc.model, voice: sc.voice}
}

// SonicClient returns the underlying Bedrock client.
func (p *SonicProvider) SonicClient() *bedrockruntime.Client { return p.client }

// Session opens a bidirectional Nova Sonic session.
//
// The session immediately sends sessionStart. The first audio chunk
// (or SignalEOT) sent via [SonicSession.Send] auto-initialises the prompt.
func (p *SonicProvider) Session(ctx context.Context, cfg niro.RealtimeConfig) (niro.RealtimeSession, error) {
	model := cfg.Model
	if model == "" {
		model = p.model
	}
	voice := cfg.Voice
	if voice == "" {
		voice = p.voice
	}
	if cfg.InputFormat == "" {
		cfg.InputFormat = niro.AudioPCM16k
	}
	if cfg.OutputFormat == "" {
		cfg.OutputFormat = niro.AudioPCM24k
	}

	out, err := p.client.InvokeModelWithBidirectionalStream(ctx,
		&bedrockruntime.InvokeModelWithBidirectionalStreamInput{
			ModelId: aws.String(model),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("sonic: open stream: %w", err)
	}

	recv, recvEmit := niro.NewStream(niro.RealtimeStreamBuffer)
	sess := &SonicSession{
		cfg:      cfg,
		voice:    voice,
		es:       out.GetStream(),
		recv:     recv,
		recvEmit: recvEmit,
		closed:   make(chan struct{}),
	}

	// Send sessionStart immediately.
	if err := sess.sendEvent(sonicEvent{Event: sonicEventInner{
		SessionStart: &sonicSessionStart{
			InferenceConfiguration: sonicInferenceConfig{
				MaxTokens:   sess.cfg.Options.MaxTokens,
				Temperature: floatPtrVal(sess.cfg.Options.Temperature),
				TopP:        floatPtrVal(sess.cfg.Options.TopP),
			},
		},
	}}); err != nil {
		out.GetStream().Close()
		return nil, fmt.Errorf("sonic: sessionStart: %w", err)
	}

	// Start the receive loop.
	go sess.recvLoop()

	return sess, nil
}

// ────────────────────────────────────────────────────────────────────────────
// SonicSession
// ────────────────────────────────────────────────────────────────────────────

// SonicSession is a live Nova Sonic bidirectional speech session.
type SonicSession struct {
	cfg   niro.RealtimeConfig
	voice string
	es    *bedrockruntime.InvokeModelWithBidirectionalStreamEventStream

	// Receive stream (written by recvLoop, read by the caller).
	recv     *niro.Stream
	recvEmit *niro.Emitter

	// mu protects promptID, audioContentID, eotSent, systemSent, pendingTool.
	mu sync.Mutex

	// promptMu serialises ensurePromptOpen so only one goroutine initialises
	// a prompt at a time. This prevents the TOCTOU where two concurrent senders
	// both see promptID=="" and both send promptStart.
	promptMu sync.Mutex

	promptID       string // current prompt UUID; "" = no active prompt
	audioContentID string // UUID of the open AUDIO content block; "" = none
	eotSent        bool   // promptEnd has been sent for current promptID
	systemSent     bool   // system prompt sent at least once
	pendingTool    *sonicPendingTool

	closeOnce sync.Once
	closed    chan struct{}
	err       atomic.Pointer[error]
}

type sonicPendingTool struct {
	toolUseID string
	name      string
	argsJSON  json.RawMessage
}

// ── Public API ───────────────────────────────────────────────────────────────

// Send sends a frame to Nova Sonic.
func (s *SonicSession) Send(ctx context.Context, f niro.Frame) error {
	switch f.Kind {
	case niro.KindAudio:
		return s.sendAudio(ctx, f.Data)
	case niro.KindText:
		return s.sendText(ctx, f.Text)
	case niro.KindToolResult:
		if f.Result == nil {
			return errors.New("sonic: Send: nil ToolResult")
		}
		return s.sendToolResult(ctx, f.Result)
	case niro.KindControl:
		switch f.Signal {
		case niro.SignalEOT:
			return s.sendEOT(ctx)
		case niro.SignalAbort:
			return s.sendAbort(ctx)
		}
	}
	return nil // silently ignore unsupported kinds
}

// Recv returns the model output stream.
func (s *SonicSession) Recv() *niro.Stream { return s.recv }

// Close terminates the session, flushing contentEnd → promptEnd → sessionEnd
// before closing the underlying event stream.
//
// sendForce is used here instead of sendEvent because s.closed is signalled
// first (to block new user sends), which would make sendEvent return ErrClosed.
func (s *SonicSession) Close() error {
	var firstErr error
	s.closeOnce.Do(func() {
		// Block new user sends first.
		close(s.closed)

		// Flush any open audio content block.
		s.mu.Lock()
		pid, cid := s.promptID, s.audioContentID
		s.audioContentID = ""
		s.mu.Unlock()
		if cid != "" {
			_ = s.sendForce(sonicEvent{Event: sonicEventInner{
				ContentEnd: &sonicContentEnd{PromptName: pid, ContentName: cid},
			}})
		}

		// Close any open prompt.
		s.mu.Lock()
		pid2, eot := s.promptID, s.eotSent
		s.promptID = ""
		s.mu.Unlock()
		if pid2 != "" && !eot {
			_ = s.sendForce(sonicEvent{Event: sonicEventInner{
				PromptEnd: &sonicPromptEnd{PromptName: pid2},
			}})
		}

		// sessionEnd — always required by the Nova Sonic protocol.
		firstErr = s.sendForce(sonicEvent{Event: sonicEventInner{SessionEnd: &struct{}{}}})

		if err := s.es.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	})
	return firstErr
}

// Err returns the session error.
func (s *SonicSession) Err() error {
	if ep := s.err.Load(); ep != nil {
		return *ep
	}
	return nil
}

// ── Send helpers ─────────────────────────────────────────────────────────────

func (s *SonicSession) sendAudio(ctx context.Context, data []byte) error {
	if err := s.ensurePromptOpen(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	var promptID, contentID string
	if s.audioContentID == "" {
		// Claim the content block under the lock so no concurrent caller opens another.
		contentID = uuid.NewString()
		s.audioContentID = contentID
		promptID = s.promptID
		s.mu.Unlock()

		if err := s.sendEvent(sonicEvent{Event: sonicEventInner{
			ContentStart: &sonicContentStart{
				PromptName:  promptID,
				ContentName: contentID,
				Type:        "AUDIO",
				Interactive: true,
				Role:        "USER",
				AudioInputConfiguration: &sonicAudioInputConfig{
					MediaType:       "audio/lpcm",
					SampleRateHertz: 16000,
					SampleSizeBits:  16,
					ChannelCount:    1,
					AudioType:       "SPEECH",
					Encoding:        "base64",
				},
			},
		}}); err != nil {
			// Roll back the claim so the next call can retry.
			s.mu.Lock()
			if s.audioContentID == contentID {
				s.audioContentID = ""
			}
			s.mu.Unlock()
			return err
		}
	} else {
		contentID = s.audioContentID
		promptID = s.promptID
		s.mu.Unlock()
	}

	// Hot path: pool-based JSON + base64 encoding — no reflection, no string alloc.
	payload := sonicEncodeAudioEvent(promptID, contentID, data)
	return s.es.Writer.Send(ctx,
		&bedrocktypes.InvokeModelWithBidirectionalStreamInputMemberChunk{
			Value: bedrocktypes.BidirectionalInputPayloadPart{Bytes: payload},
		})
}

func (s *SonicSession) sendText(ctx context.Context, text string) error {
	if err := s.ensurePromptOpen(ctx); err != nil {
		return err
	}

	contentID := uuid.NewString()
	s.mu.Lock()
	promptID := s.promptID
	s.mu.Unlock()

	if err := s.sendEvent(sonicEvent{Event: sonicEventInner{
		ContentStart: &sonicContentStart{
			PromptName:  promptID,
			ContentName: contentID,
			Type:        "TEXT",
			Interactive: false,
			Role:        "USER",
			TextInputConfiguration: &sonicTextInputConfig{
				MediaType: "text/plain",
			},
		},
	}}); err != nil {
		return err
	}
	if err := s.sendEvent(sonicEvent{Event: sonicEventInner{
		TextInput: &sonicTextInput{
			PromptName:  promptID,
			ContentName: contentID,
			Content:     text,
		},
	}}); err != nil {
		return err
	}
	return s.sendEvent(sonicEvent{Event: sonicEventInner{
		ContentEnd: &sonicContentEnd{PromptName: promptID, ContentName: contentID},
	}})
}

// sendEOT closes the audio block and sends promptEnd, triggering model generation.
func (s *SonicSession) sendEOT(ctx context.Context) error {
	s.mu.Lock()
	promptID := s.promptID
	audioContentID := s.audioContentID

	if promptID == "" {
		s.mu.Unlock()
		return nil // nothing to end
	}
	if s.eotSent {
		s.mu.Unlock()
		return nil // already sent
	}
	s.audioContentID = ""
	s.eotSent = true
	s.mu.Unlock()

	// Close audio content block if open.
	if audioContentID != "" {
		if err := s.sendEvent(sonicEvent{Event: sonicEventInner{
			ContentEnd: &sonicContentEnd{PromptName: promptID, ContentName: audioContentID},
		}}); err != nil {
			return err
		}
	}

	// Send promptEnd — tells Nova Sonic to generate a response.
	return s.sendEvent(sonicEvent{Event: sonicEventInner{
		PromptEnd: &sonicPromptEnd{PromptName: promptID},
	}})
}

// sendAbort cancels an in-progress generation (barge-in).
// Nova Sonic doesn't have an explicit "cancel response" event; we signal
// end-of-turn and immediately open a fresh prompt.
func (s *SonicSession) sendAbort(ctx context.Context) error {
	return s.sendEOT(ctx)
}

// sendToolResult sends the result of a tool call back to Nova Sonic.
// Must be called after receiving a KindToolCall frame from Recv.
func (s *SonicSession) sendToolResult(ctx context.Context, result *niro.ToolResult) error {
	s.mu.Lock()
	promptID := s.promptID
	pending := s.pendingTool
	if pending != nil && pending.toolUseID == result.CallID {
		s.pendingTool = nil
	}
	s.mu.Unlock()

	if promptID == "" {
		return errors.New("sonic: no active prompt for tool result")
	}

	contentID := uuid.NewString()

	if err := s.sendEvent(sonicEvent{Event: sonicEventInner{
		ContentStart: &sonicContentStart{
			PromptName:  promptID,
			ContentName: contentID,
			Interactive: false,
			Type:        "TOOL",
			Role:        "TOOL",
			ToolResultInputConfiguration: &sonicToolResultInputConfig{
				ToolUseID: result.CallID,
				Type:      "TEXT",
				TextInputConfiguration: &sonicTextInputConfig{
					MediaType: "text/plain",
				},
			},
		},
	}}); err != nil {
		return err
	}

	content := result.Content
	if result.IsError {
		content = "ERROR: " + content
	}
	if err := s.sendEvent(sonicEvent{Event: sonicEventInner{
		ToolResult: &sonicToolResult{
			PromptName:  promptID,
			ContentName: contentID,
			Content:     content,
		},
	}}); err != nil {
		return err
	}
	return s.sendEvent(sonicEvent{Event: sonicEventInner{
		ContentEnd: &sonicContentEnd{PromptName: promptID, ContentName: contentID},
	}})
}

// ensurePromptOpen opens a new prompt (with optional system prompt) if none is active.
//
// Double-checked locking pattern prevents the TOCTOU race where two concurrent
// callers both see promptID=="" and both send promptStart:
//  1. Fast path under mu — returns if promptID is already set.
//  2. Acquire promptMu (serialises competing openers).
//  3. Re-check under mu — another goroutine may have opened while we waited.
//  4. Send all prompt events without holding mu (avoids deadlock with recvLoop).
//  5. Commit: set promptID under mu.
func (s *SonicSession) ensurePromptOpen(ctx context.Context) error {
	// 1. Fast path.
	s.mu.Lock()
	if s.promptID != "" {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	// 2. Serialise competing openers.
	s.promptMu.Lock()
	defer s.promptMu.Unlock()

	// 3. Double-check after acquiring promptMu.
	s.mu.Lock()
	if s.promptID != "" {
		s.mu.Unlock()
		return nil
	}
	needSysPrompt := !s.systemSent && s.cfg.SystemPrompt != ""
	if needSysPrompt {
		s.systemSent = true
	}
	s.mu.Unlock()

	promptID := uuid.NewString()

	// 4. Build and send promptStart (no lock held during I/O).
	ps := &sonicPromptStart{
		PromptName: promptID,
		TextOutputConfiguration: &sonicTextOutputConfig{
			MediaType: "text/plain",
		},
		AudioOutputConfiguration: &sonicAudioOutputConfig{
			MediaType:       "audio/lpcm",
			SampleRateHertz: 24000,
			SampleSizeBits:  16,
			ChannelCount:    1,
			VoiceID:         s.voice,
			Encoding:        "base64",
			AudioType:       "SPEECH",
		},
		ToolUseOutputConfiguration: &sonicToolUseOutputConfig{
			MediaType: "application/json",
		},
	}
	if len(s.cfg.Tools) > 0 {
		ps.ToolConfiguration = buildSonicToolConfig(s.cfg.Tools)
	}

	if err := s.sendEvent(sonicEvent{Event: sonicEventInner{PromptStart: ps}}); err != nil {
		if needSysPrompt {
			s.mu.Lock()
			s.systemSent = false // roll back so the next attempt re-sends the system prompt
			s.mu.Unlock()
		}
		return err
	}

	if needSysPrompt {
		sysContentID := uuid.NewString()
		if err := s.sendEvent(sonicEvent{Event: sonicEventInner{
			ContentStart: &sonicContentStart{
				PromptName:  promptID,
				ContentName: sysContentID,
				Type:        "TEXT",
				Interactive: false,
				Role:        "SYSTEM",
				TextInputConfiguration: &sonicTextInputConfig{
					MediaType: "text/plain",
				},
			},
		}}); err != nil {
			return err
		}
		if err := s.sendEvent(sonicEvent{Event: sonicEventInner{
			TextInput: &sonicTextInput{
				PromptName:  promptID,
				ContentName: sysContentID,
				Content:     s.cfg.SystemPrompt,
			},
		}}); err != nil {
			return err
		}
		if err := s.sendEvent(sonicEvent{Event: sonicEventInner{
			ContentEnd: &sonicContentEnd{PromptName: promptID, ContentName: sysContentID},
		}}); err != nil {
			return err
		}
	}

	// 5. Commit.
	s.mu.Lock()
	s.promptID = promptID
	s.eotSent = false
	s.mu.Unlock()
	return nil
}

// ── Receive loop ─────────────────────────────────────────────────────────────

func (s *SonicSession) recvLoop() {
	defer func() {
		// Drain any pending tool state before closing.
		s.recvEmit.Close()
	}()

	var (
		// inToolBlock / toolUseBuffer track tool call state across contentStart → toolUse → contentEnd.
		inToolBlock   bool
		toolUseBuffer sonicToolUseEvent
		// textStage tracks the current content block's generation stage.
		// Nova Sonic emits SPECULATIVE (low-latency preview) frames before FINAL committed frames.
		// Only FINAL text is emitted to the caller; SPECULATIVE frames are discarded.
		textStage string
	)

	for {
		select {
		case <-s.closed:
			return
		case ev, ok := <-s.es.Reader.Events():
			if !ok {
				if err := s.es.Reader.Err(); err != nil {
					s.storeErr(err)
					s.recvEmit.Error(err)
				}
				return
			}

			chunk, ok := ev.(*bedrocktypes.InvokeModelWithBidirectionalStreamOutputMemberChunk)
			if !ok {
				continue
			}

			var resp sonicResponseEvent
			if err := json.Unmarshal(chunk.Value.Bytes, &resp); err != nil {
				// Malformed JSON is non-fatal; log and continue.
				continue
			}

			ev2 := resp.Event

			// --- completionStart: new response beginning ---
			// (no action needed)

			// --- contentStart: record generation stage and tool-block flag ---
			if ev2.ContentStart != nil {
				cs := ev2.ContentStart
				textStage = "" // reset for each new content block
				if cs.AdditionalModelFields != "" {
					var fields struct {
						GenerationStage string `json:"generationStage"`
					}
					_ = json.Unmarshal([]byte(cs.AdditionalModelFields), &fields)
					textStage = fields.GenerationStage
				}
				if cs.Type == "TOOL" && cs.Role == "ASSISTANT" {
					inToolBlock = true
					toolUseBuffer = sonicToolUseEvent{}
				}
			}

			// --- textOutput: suppress SPECULATIVE frames, only emit FINAL ---
			if ev2.TextOutput != nil {
				to := ev2.TextOutput
				// Discard low-latency speculative preview frames.
				if textStage == "SPECULATIVE" {
					continue
				}
				if to.Content == `{ "interrupted" : true }` {
					_ = s.recvEmit.Emit(context.Background(), niro.ControlFrame(niro.SignalFlush))
					continue
				}
				if to.Content != "" {
					_ = s.recvEmit.Emit(context.Background(), niro.TextFrame(to.Content))
				}
			}

			// --- audioOutput ---
			if ev2.AudioOutput != nil {
				pcm, err := base64.StdEncoding.DecodeString(ev2.AudioOutput.Content)
				if err == nil && len(pcm) > 0 {
					_ = s.recvEmit.Emit(context.Background(),
						niro.AudioFrame(pcm, niro.AudioPCM24k))
				}
			}

			// --- toolUse ---
			if ev2.ToolUse != nil {
				toolUseBuffer = *ev2.ToolUse
			}

			// --- contentEnd ---
			if ev2.ContentEnd != nil {
				ce := ev2.ContentEnd
				if ce.Type == "TOOL" && inToolBlock && toolUseBuffer.ToolUseID != "" {
					// Complete tool call received.
					inToolBlock = false
					argsJSON := json.RawMessage(toolUseBuffer.Content)
					if !json.Valid(argsJSON) {
						argsJSON = []byte(`{}`)
					}
					call := &niro.ToolCall{
						ID:   toolUseBuffer.ToolUseID,
						Name: toolUseBuffer.ToolName,
						Args: argsJSON,
					}
					s.mu.Lock()
					s.pendingTool = &sonicPendingTool{
						toolUseID: call.ID,
						name:      call.Name,
						argsJSON:  argsJSON,
					}
					s.mu.Unlock()
					_ = s.recvEmit.Emit(context.Background(), niro.ToolCallFrame(call))
				}
			}

			// --- completionEnd: model finished this turn ---
			if ev2.CompletionEnd != nil {
				// Emit EOT signal and reset prompt state for the next turn.
				_ = s.recvEmit.Emit(context.Background(), niro.ControlFrame(niro.SignalEOT))
				s.mu.Lock()
				s.promptID = ""
				s.audioContentID = ""
				s.eotSent = false
				s.mu.Unlock()
			}

			// --- usageEvent ---
			if ev2.UsageEvent != nil {
				u := &niro.Usage{
					InputTokens:  ev2.UsageEvent.InputTokens,
					OutputTokens: ev2.UsageEvent.OutputTokens,
					TotalTokens:  ev2.UsageEvent.InputTokens + ev2.UsageEvent.OutputTokens,
				}
				_ = s.recvEmit.Emit(context.Background(), niro.UsageFrame(u))
			}
		}
	}
}

// ── Low-level send helpers ────────────────────────────────────────────────────

// sendEvent marshals ev and writes it to the event stream, returning
// niro.ErrClosed if the session has already been closed.
// Used for all normal (non-Close) sends.
func (s *SonicSession) sendEvent(ev sonicEvent) error {
	select {
	case <-s.closed:
		return niro.ErrClosed
	default:
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("sonic: marshal event: %w", err)
	}
	return s.es.Writer.Send(context.Background(),
		&bedrocktypes.InvokeModelWithBidirectionalStreamInputMemberChunk{
			Value: bedrocktypes.BidirectionalInputPayloadPart{Bytes: data},
		})
}

// sendForce marshals ev and writes it unconditionally, bypassing the s.closed
// check. Used exclusively in Close() to flush graceful termination events
// after s.closed has been signalled — sendEvent would return ErrClosed there.
func (s *SonicSession) sendForce(ev sonicEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("sonic: marshal event: %w", err)
	}
	return s.es.Writer.Send(context.Background(),
		&bedrocktypes.InvokeModelWithBidirectionalStreamInputMemberChunk{
			Value: bedrocktypes.BidirectionalInputPayloadPart{Bytes: data},
		})
}

func (s *SonicSession) storeErr(err error) {
	s.err.CompareAndSwap(nil, &err)
}

// ── Tool config builder ───────────────────────────────────────────────────────

func buildSonicToolConfig(tools []niro.Tool) *sonicToolConfiguration {
	specs := make([]sonicToolSpec, len(tools))
	for i, t := range tools {
		schemaStr := string(t.Parameters)
		if !json.Valid(t.Parameters) {
			schemaStr = `{}`
		}
		specs[i] = sonicToolSpec{
			ToolSpec: sonicToolSpecInner{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: sonicToolInputSchema{JSON: schemaStr},
			},
		}
	}
	return &sonicToolConfiguration{Tools: specs}
}

func floatPtrVal(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// ────────────────────────────────────────────────────────────────────────────
// JSON event types — client → Nova Sonic
// ────────────────────────────────────────────────────────────────────────────

type sonicEvent struct {
	Event sonicEventInner `json:"event"`
}

type sonicEventInner struct {
	SessionStart *sonicSessionStart `json:"sessionStart,omitempty"`
	PromptStart  *sonicPromptStart  `json:"promptStart,omitempty"`
	ContentStart *sonicContentStart `json:"contentStart,omitempty"`
	TextInput    *sonicTextInput    `json:"textInput,omitempty"`
	AudioInput   *sonicAudioInput   `json:"audioInput,omitempty"`
	ToolResult   *sonicToolResult   `json:"toolResult,omitempty"`
	ContentEnd   *sonicContentEnd   `json:"contentEnd,omitempty"`
	PromptEnd    *sonicPromptEnd    `json:"promptEnd,omitempty"`
	SessionEnd   *struct{}          `json:"sessionEnd,omitempty"`
}

type sonicSessionStart struct {
	InferenceConfiguration sonicInferenceConfig `json:"inferenceConfiguration"`
}

type sonicInferenceConfig struct {
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"topP,omitempty"`
}

type sonicPromptStart struct {
	PromptName                 string                    `json:"promptName"`
	TextOutputConfiguration    *sonicTextOutputConfig    `json:"textOutputConfiguration,omitempty"`
	AudioOutputConfiguration   *sonicAudioOutputConfig   `json:"audioOutputConfiguration,omitempty"`
	ToolUseOutputConfiguration *sonicToolUseOutputConfig `json:"toolUseOutputConfiguration,omitempty"`
	ToolConfiguration          *sonicToolConfiguration   `json:"toolConfiguration,omitempty"`
}

type sonicTextOutputConfig struct {
	MediaType string `json:"mediaType"`
}

type sonicAudioOutputConfig struct {
	MediaType       string `json:"mediaType"`
	SampleRateHertz int    `json:"sampleRateHertz"`
	SampleSizeBits  int    `json:"sampleSizeBits"`
	ChannelCount    int    `json:"channelCount"`
	VoiceID         string `json:"voiceId"`
	Encoding        string `json:"encoding"`
	AudioType       string `json:"audioType"`
}

type sonicToolUseOutputConfig struct {
	MediaType string `json:"mediaType"`
}

type sonicToolConfiguration struct {
	Tools []sonicToolSpec `json:"tools"`
}

type sonicToolSpec struct {
	ToolSpec sonicToolSpecInner `json:"toolSpec"`
}

type sonicToolSpecInner struct {
	Name        string               `json:"name"`
	Description string               `json:"description"`
	InputSchema sonicToolInputSchema `json:"inputSchema"`
}

// sonicToolInputSchema wraps the JSON Schema as a string — Nova Sonic requires
// the schema serialised as a JSON string value, not an embedded object.
type sonicToolInputSchema struct {
	JSON string `json:"json"`
}

type sonicContentStart struct {
	PromptName                   string                      `json:"promptName"`
	ContentName                  string                      `json:"contentName"`
	Type                         string                      `json:"type"`
	Interactive                  bool                        `json:"interactive"`
	Role                         string                      `json:"role"`
	TextInputConfiguration       *sonicTextInputConfig       `json:"textInputConfiguration,omitempty"`
	AudioInputConfiguration      *sonicAudioInputConfig      `json:"audioInputConfiguration,omitempty"`
	ToolResultInputConfiguration *sonicToolResultInputConfig `json:"toolResultInputConfiguration,omitempty"`
}

type sonicTextInputConfig struct {
	MediaType string `json:"mediaType"`
}

type sonicAudioInputConfig struct {
	MediaType       string `json:"mediaType"`
	SampleRateHertz int    `json:"sampleRateHertz"`
	SampleSizeBits  int    `json:"sampleSizeBits"`
	ChannelCount    int    `json:"channelCount"`
	AudioType       string `json:"audioType"`
	Encoding        string `json:"encoding"`
}

type sonicToolResultInputConfig struct {
	ToolUseID              string                `json:"toolUseId"`
	Type                   string                `json:"type"`
	TextInputConfiguration *sonicTextInputConfig `json:"textInputConfiguration,omitempty"`
}

type sonicTextInput struct {
	PromptName  string `json:"promptName"`
	ContentName string `json:"contentName"`
	Content     string `json:"content"`
}

type sonicAudioInput struct {
	PromptName  string `json:"promptName"`
	ContentName string `json:"contentName"`
	Content     string `json:"content"` // base64-encoded PCM16 @ 16kHz
}

type sonicToolResult struct {
	PromptName  string `json:"promptName"`
	ContentName string `json:"contentName"`
	Content     string `json:"content"`
}

type sonicContentEnd struct {
	PromptName  string `json:"promptName"`
	ContentName string `json:"contentName"`
}

type sonicPromptEnd struct {
	PromptName string `json:"promptName"`
}

// ────────────────────────────────────────────────────────────────────────────
// JSON event types — Nova Sonic → client
// ────────────────────────────────────────────────────────────────────────────

type sonicResponseEvent struct {
	Event sonicResponseInner `json:"event"`
}

type sonicResponseInner struct {
	CompletionStart *struct{}              `json:"completionStart,omitempty"`
	ContentStart    *sonicRespContentStart `json:"contentStart,omitempty"`
	TextOutput      *sonicTextOutputEvent  `json:"textOutput,omitempty"`
	AudioOutput     *sonicAudioOutputEvent `json:"audioOutput,omitempty"`
	ToolUse         *sonicToolUseEvent     `json:"toolUse,omitempty"`
	ContentEnd      *sonicRespContentEnd   `json:"contentEnd,omitempty"`
	CompletionEnd   *struct{}              `json:"completionEnd,omitempty"`
	UsageEvent      *sonicUsageEvent       `json:"usageEvent,omitempty"`
}

type sonicRespContentStart struct {
	Role                  string `json:"role"`
	ContentName           string `json:"contentName"`
	Type                  string `json:"type"`
	AdditionalModelFields string `json:"additionalModelFields"` // JSON string: {"generationStage":"SPECULATIVE"|"FINAL"}
}

type sonicTextOutputEvent struct {
	Content string `json:"content"`
	Role    string `json:"role"`
}

type sonicAudioOutputEvent struct {
	Content string `json:"content"` // base64-encoded PCM16 @ 24kHz
}

type sonicToolUseEvent struct {
	ToolName  string `json:"toolName"`
	ToolUseID string `json:"toolUseId"`
	Content   string `json:"content"` // JSON-encoded arguments
}

type sonicRespContentEnd struct {
	PromptName  string `json:"promptName"`
	ContentName string `json:"contentName"`
	Type        string `json:"type"` // "TEXT" | "AUDIO" | "TOOL"
}

type sonicUsageEvent struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}
