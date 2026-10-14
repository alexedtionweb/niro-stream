package realtime

import (
	"context"
	"encoding/json"

	"github.com/alexedtionweb/niro-stream"
)

func (s *Session) handleMessage(msg []byte) bool {
	top := scanTop(msg)
	if top.seq > 0 {
		if top.seq <= s.lastSeq.Load() {
			return true
		}
		s.lastSeq.Store(top.seq)
		s.maybeAck(top.seq)
	}

	if top.audioDelta() {
		s.emitAudio(top)
		return true
	}
	if top.textDelta() {
		if p := top.payload(); len(p) > 0 {
			_ = s.recvEmit.Emit(context.Background(), niro.TextFrame(string(p)))
		}
		return true
	}

	var ev serverEvent
	if niro.JSONUnmarshal(msg, &ev) != nil {
		return true
	}
	return s.handleCold(ev)
}

func (s *Session) emitAudio(top topFields) {
	b64 := top.delta
	if len(b64) == 0 {
		b64 = top.audio
	}
	if len(b64) == 0 {
		return
	}
	pcm, err := decodeB64(nil, b64)
	if err != nil || len(pcm) == 0 {
		return
	}
	rate := top.rate
	if rate <= 0 {
		rate = s.outputRate
	}
	format := ""
	if len(top.format) > 0 {
		format = string(top.format)
	}
	_ = s.recvEmit.Emit(context.Background(), niro.AudioFrame(pcm, pcmMIME(rate, format)))
}

func (s *Session) maybeAck(seq int64) {
	if s.dialect != dialectVLLM {
		return
	}
	if seq-s.lastAck.Load() < ackEvery {
		return
	}
	s.lastAck.Store(seq)
	payload := []byte(`{"type":"session.event_ack","server_event_seq":`)
	payload = append(strconvAppend(payload, seq), '}')
	select {
	case <-s.closed:
	case s.sendCh <- outMsg{b: payload}:
	default:
	}
}

func strconvAppend(dst []byte, n int64) []byte {
	return appendInt(dst, n)
}

func appendInt(dst []byte, n int64) []byte {
	if n == 0 {
		return append(dst, '0')
	}
	var tmp [20]byte
	i := len(tmp)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		tmp[i] = '-'
	}
	return append(dst, tmp[i:]...)
}

func (s *Session) handleCold(ev serverEvent) bool {
	ctx := context.Background()
	switch ev.Type {
	case "session.created", "session.updated", "session.resumed":
		s.absorbSession(ev)
	case "input_audio_buffer.speech_started":
		_ = s.recvEmit.Emit(ctx, niro.ControlFrame(niro.SignalFlush))
	case "response.function_call_arguments.delta":
		if ev.CallID == "" {
			return true
		}
		s.funcArgsMu.Lock()
		acc := s.funcArgs[ev.CallID]
		if acc == nil {
			acc = &funcCallAccum{}
			s.funcArgs[ev.CallID] = acc
		}
		acc.args.WriteString(ev.Delta)
		if ev.Name != "" {
			acc.name = ev.Name
		}
		s.funcArgsMu.Unlock()
	case "response.function_call_arguments.done":
		s.emitToolCall(ev.CallID, ev.Name, ev.Arguments)
	case "response.output_item.added", "response.output_item.done":
		s.noteOutputItem(ev)
	case "conversation.item.input_audio_transcription.completed", "transcription.completed":
		text := ev.Transcript
		if text == "" {
			text = ev.Delta
		}
		if text == "" {
			return true
		}
		_ = s.recvEmit.Emit(ctx, niro.Frame{
			Kind:   niro.KindCustom,
			Text:   text,
			Custom: &niro.ExperimentalFrame{Type: EventInputTranscript},
		})
	case "response.speak":
		s.emitDecision(EventSpeak, ev.ResponseID)
	case "response.listen":
		s.emitDecision(EventListen, ev.ResponseID)
	case "overlap.decision":
		_ = s.recvEmit.Emit(ctx, niro.CustomFrame(&niro.ExperimentalFrame{
			Type: EventOverlap,
			Data: string(ev.RawDecision),
		}))
	case "playback.acknowledged":
		_ = s.recvEmit.Emit(ctx, niro.CustomFrame(&niro.ExperimentalFrame{
			Type: EventPlaybackAcked,
			Data: ev.Playback,
		}))
	case "response.done":
		s.emitResponseDone(ev)
	case "error":
		msg, code := errorText(ev.Error)
		err := niro.NewErrorf(niro.ErrCodeProviderError, "server error [%s]: %s", code, msg)
		s.storeErr(err)
		s.recvEmit.Error(err)
		return false
	case "session.closed", "session.expired", "session.replaced", "session.resync_required":
		if ev.Type != "session.closed" || (ev.Reason != "" && ev.Reason != "client_close") {
			err := niro.NewErrorf(niro.ErrCodeProviderError, "%s: %s", ev.Type, ev.Reason)
			s.storeErr(err)
			s.recvEmit.Error(err)
		}
		return false
	}
	return true
}

func (s *Session) emitDecision(kind, responseID string) {
	_ = s.recvEmit.Emit(context.Background(), niro.CustomFrame(&niro.ExperimentalFrame{
		Type: kind,
		Data: responseID,
	}))
}

func (s *Session) emitToolCall(callID, name, args string) {
	if callID == "" {
		return
	}
	s.funcArgsMu.Lock()
	acc := s.funcArgs[callID]
	if acc != nil {
		delete(s.funcArgs, callID)
	}
	s.funcArgsMu.Unlock()
	if args == "" && acc != nil {
		args = acc.args.String()
	}
	if name == "" && acc != nil {
		name = acc.name
	}
	if name == "" {
		s.funcArgsMu.Lock()
		name = s.funcNames[callID]
		delete(s.funcNames, callID)
		s.funcArgsMu.Unlock()
	}
	s.funcArgsMu.Lock()
	if _, sent := s.funcSent[callID]; sent {
		s.funcArgsMu.Unlock()
		return
	}
	if s.funcSent == nil {
		s.funcSent = make(map[string]struct{}, 4)
	}
	s.funcSent[callID] = struct{}{}
	s.funcArgsMu.Unlock()

	raw := json.RawMessage(args)
	if len(raw) == 0 || !json.Valid(raw) {
		raw = []byte(`{}`)
	}
	_ = s.recvEmit.Emit(context.Background(), niro.ToolCallFrame(&niro.ToolCall{
		ID:   callID,
		Name: name,
		Args: raw,
	}))
}

func (s *Session) noteOutputItem(ev serverEvent) {
	if len(ev.Item) == 0 {
		return
	}
	var item struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
		Args   string `json:"arguments"`
	}
	if niro.JSONUnmarshal(ev.Item, &item) != nil || item.Type != "function_call" || item.CallID == "" {
		return
	}
	s.funcArgsMu.Lock()
	if item.Name != "" {
		s.funcNames[item.CallID] = item.Name
		if acc := s.funcArgs[item.CallID]; acc != nil && acc.name == "" {
			acc.name = item.Name
		}
	}
	s.funcArgsMu.Unlock()
	if ev.Type == "response.output_item.done" && item.Args != "" {
		s.emitToolCall(item.CallID, item.Name, item.Args)
	}
}

func (s *Session) emitResponseDone(ev serverEvent) {
	ctx := context.Background()
	var resp struct {
		Status        string `json:"status"`
		StatusDetails struct {
			Reason string `json:"reason"`
		} `json:"status_details"`
		Usage *struct {
			TotalTokens        int           `json:"total_tokens"`
			InputTokens        int           `json:"input_tokens"`
			OutputTokens       int           `json:"output_tokens"`
			InputTokenDetails  *tokenDetails `json:"input_token_details"`
			OutputTokenDetails *tokenDetails `json:"output_token_details"`
		} `json:"usage"`
	}
	if len(ev.Response) > 0 {
		_ = niro.JSONUnmarshal(ev.Response, &resp)
	}
	if resp.Usage != nil {
		u := &niro.Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.TotalTokens,
			Detail:       map[string]int{},
		}
		if d := resp.Usage.InputTokenDetails; d != nil {
			u.Detail["input_audio_tokens"] = d.AudioTokens
			u.Detail["input_text_tokens"] = d.TextTokens
			u.Detail["input_cached_tokens"] = d.CachedTokens
		}
		if d := resp.Usage.OutputTokenDetails; d != nil {
			u.Detail["output_audio_tokens"] = d.AudioTokens
			u.Detail["output_text_tokens"] = d.TextTokens
		}
		_ = s.recvEmit.Emit(ctx, niro.UsageFrame(u))
	}
	if resp.Status == "failed" {
		reason := resp.StatusDetails.Reason
		if reason == "" {
			reason = "failed"
		}
		_ = s.recvEmit.Emit(ctx, niro.CustomFrame(&niro.ExperimentalFrame{
			Type: EventResponseFailed,
			Data: reason,
		}))
	}
	_ = s.recvEmit.Emit(ctx, niro.ControlFrame(niro.SignalEOT))
}

func (s *Session) absorbSession(ev serverEvent) {
	if ev.ResumeToken != "" {
		s.resumeToken = ev.ResumeToken
	}
	if len(ev.Session) == 0 {
		return
	}
	var sess struct {
		ID           string `json:"id"`
		Capabilities struct {
			SupportsSessionResume bool   `json:"supports_session_resume"`
			SupportsBargeIn       bool   `json:"supports_barge_in"`
			ChunkPeriodMs         int    `json:"chunk_period_ms"`
			ImplementationLevel   string `json:"implementation_level"`
		} `json:"capabilities"`
		Audio struct {
			Input struct {
				SampleRateHz int `json:"sample_rate_hz"`
				Format       struct {
					Rate int `json:"rate"`
				} `json:"format"`
			} `json:"input"`
			Output struct {
				Format struct {
					Rate int `json:"rate"`
				} `json:"format"`
			} `json:"output"`
		} `json:"audio"`
	}
	if niro.JSONUnmarshal(ev.Session, &sess) != nil {
		return
	}
	if sess.ID != "" {
		s.sessionID = sess.ID
	}
	s.supportsResume.Store(sess.Capabilities.SupportsSessionResume)
	s.supportsBargeIn.Store(sess.Capabilities.SupportsBargeIn)
	if s.explicitResponse {
		s.modelNative.Store(false)
	} else if sess.Capabilities.ImplementationLevel == "model_native_duplex" {
		s.modelNative.Store(true)
	}
	if rate := sess.Audio.Output.Format.Rate; rate > 0 {
		s.outputRate = rate
	}
	if rate := sess.Audio.Input.Format.Rate; rate > 0 {
		s.inputRate = rate
	} else if sess.Audio.Input.SampleRateHz > 0 {
		s.inputRate = sess.Audio.Input.SampleRateHz
	}
	if ev.Type == "session.created" {
		_ = s.recvEmit.Emit(context.Background(), niro.CustomFrame(&niro.ExperimentalFrame{
			Type: EventSession,
			Data: SessionInfo{
				ID:              s.sessionID,
				ResumeToken:     s.resumeToken,
				SupportsBargeIn: sess.Capabilities.SupportsBargeIn,
				SupportsResume:  sess.Capabilities.SupportsSessionResume,
				ChunkPeriodMs:   sess.Capabilities.ChunkPeriodMs,
				InputRate:       s.inputRate,
				OutputRate:      s.outputRate,
			},
		}))
	}
}

func errorText(raw json.RawMessage) (msg, code string) {
	if len(raw) == 0 {
		return "unknown", ""
	}
	if raw[0] == '"' {
		var s string
		if niro.JSONUnmarshal(raw, &s) == nil {
			return s, ""
		}
	}
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if niro.JSONUnmarshal(raw, &e) == nil {
		if e.Message == "" {
			e.Message = string(raw)
		}
		return e.Message, e.Code
	}
	return string(raw), ""
}

type serverEvent struct {
	Type        string          `json:"type"`
	Delta       string          `json:"delta"`
	Audio       string          `json:"audio"`
	CallID      string          `json:"call_id"`
	Name        string          `json:"name"`
	Arguments   string          `json:"arguments"`
	Transcript  string          `json:"transcript"`
	ResponseID  string          `json:"response_id"`
	Response    json.RawMessage `json:"response"`
	Item        json.RawMessage `json:"item"`
	Error       json.RawMessage `json:"error"`
	Session     json.RawMessage `json:"session"`
	ResumeToken string          `json:"resume_token"`
	Reason      string          `json:"reason"`
	Playback    json.RawMessage `json:"playback"`
	RawDecision json.RawMessage `json:"metadata"`
}

type tokenDetails struct {
	AudioTokens  int `json:"audio_tokens"`
	TextTokens   int `json:"text_tokens"`
	CachedTokens int `json:"cached_tokens"`
}
