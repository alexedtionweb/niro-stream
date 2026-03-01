package niro

import "context"

// RealtimeProvider creates long-lived bidirectional speech sessions.
// Unlike [Provider] (unidirectional request-response), RealtimeProvider
// supports continuous audio streaming in both directions simultaneously —
// essential for voice assistants, telephony agents, and live translation.
//
// Built-in implementations:
//   - provider/bedrock [SonicProvider] — Amazon Nova Sonic (16 kHz in, 24 kHz out)
//   - provider/realtime [Provider]     — OpenAI Realtime API (24 kHz in/out)
type RealtimeProvider interface {
	Session(ctx context.Context, cfg RealtimeConfig) (RealtimeSession, error)
}

// RealtimeSession is a live bidirectional speech session.
//
// Send and Recv operate concurrently. Run the audio-sending loop in a
// separate goroutine from the receiving loop.
//
//	sess, err := provider.Session(ctx, niro.RealtimeConfig{
//	    SystemPrompt: "You are a helpful voice assistant.",
//	})
//	defer sess.Close()
//
//	// --- send goroutine ---
//	go func() {
//	    for chunk := range mic.Chunks() {
//	        if err := sess.Send(ctx, niro.AudioFrame(chunk, niro.AudioPCM16k)); err != nil {
//	            return
//	        }
//	    }
//	    // Signal end of user turn; model will respond.
//	    sess.Send(ctx, niro.ControlFrame(niro.SignalEOT))
//	}()
//
//	// --- receive loop ---
//	out := sess.Recv()
//	for out.Next(ctx) {
//	    f := out.Frame()
//	    switch f.Kind {
//	    case niro.KindAudio:
//	        speaker.Play(f.Data)         // PCM24k synthesized speech
//	    case niro.KindText:
//	        fmt.Print(f.Text)            // transcript (when available)
//	    case niro.KindToolCall:
//	        result := executeTool(f.Tool)
//	        sess.Send(ctx, niro.ToolResultFrame(&niro.ToolResult{
//	            CallID:  f.Tool.ID,
//	            Content: result,
//	        }))
//	    case niro.KindControl:
//	        if f.Signal == niro.SignalFlush {
//	            speaker.ClearBuffer() // barge-in: user interrupted
//	        }
//	    }
//	}
type RealtimeSession interface {
	// Send sends a frame to the model.
	//
	// Supported frame kinds:
	//   - KindAudio      raw PCM audio chunk from the microphone
	//   - KindText       text message (provider-dependent support)
	//   - KindToolResult result of a tool call received via Recv
	//   - KindControl
	//       SignalEOT    end of user turn; model generates a response
	//       SignalAbort  cancel an in-progress model response (barge-in)
	Send(ctx context.Context, f Frame) error

	// Recv returns the read-only output stream from the model.
	// Call once; the stream remains open for the full session lifetime.
	//
	// Frame kinds emitted:
	//   - KindAudio     synthesized speech
	//   - KindText      transcript of the model's speech (when available)
	//   - KindToolCall  tool invocation request; respond with Send(ToolResultFrame)
	//   - KindControl
	//       SignalFlush barge-in detected; clear the audio playback buffer
	//       SignalEOT   model finished speaking this turn
	//   - KindUsage     token/audio usage at end of turn
	Recv() *Stream

	// Close terminates the session and releases all resources.
	// Safe to call multiple times and from any goroutine.
	Close() error

	// Err returns the first error that caused the session to fail.
	// Returns nil if the session closed cleanly.
	Err() error
}

// RealtimeConfig configures a bidirectional speech session.
type RealtimeConfig struct {
	// Model identifier. If empty, the provider's default model is used.
	//   Nova Sonic: "amazon.nova-sonic-v1:0"
	//   OpenAI:     "gpt-4o-realtime-preview"
	Model string

	// SystemPrompt is sent as the initial SYSTEM instruction.
	SystemPrompt string

	// Voice selects the TTS synthesis voice.
	//   Nova Sonic: "matthew", "tiffany", "amy", "brian"
	//   OpenAI:     "alloy", "echo", "fable", "nova", "onyx", "shimmer"
	Voice string

	// InputFormat is the MIME type of audio sent via Send.
	// Defaults to AudioPCM16k for Nova Sonic, AudioPCM24k for OpenAI.
	// Use the AudioPCM* constants.
	InputFormat string

	// OutputFormat is the requested audio MIME type received via Recv.
	// Defaults to AudioPCM24k (supported by all providers).
	OutputFormat string

	// Tools available for the model to call.
	// Received as KindToolCall frames from Recv.
	Tools []Tool

	// ToolChoice controls how the model selects tools.
	ToolChoice ToolChoice

	// VAD configures server-side Voice Activity Detection.
	// When nil, VAD is not configured; the caller signals turn boundaries
	// manually via Send(ControlFrame(SignalEOT)).
	VAD *VADConfig

	// Options controls generation parameters (temperature, max tokens, etc.)
	Options Options
}

// VADConfig configures server-side Voice Activity Detection.
//
// When enabled, the provider automatically detects speech boundaries and
// emits KindControl frames:
//   - SignalFlush on speech start (user is talking; clear playback buffer)
//   - SignalEOT   on speech end   (user stopped talking; model will respond)
type VADConfig struct {
	// Threshold is the activation confidence (0.0–1.0).
	// Higher values reduce false positives at the cost of onset latency.
	// Default: 0.5
	Threshold float64

	// PrefixPaddingMs is the pre-speech audio prepended to each utterance
	// to avoid clipping word onset. Default: 300 ms.
	PrefixPaddingMs int

	// SilenceDurationMs is the post-speech silence before turn-end is
	// triggered. Default: 200 ms.
	SilenceDurationMs int
}
