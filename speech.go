package niro

import "context"

// ── TTS ─────────────────────────────────────────────────────────────────────

// TTSProvider synthesizes speech from text.
//
// Unlike [Provider] (which operates on message turns), TTSProvider is a
// simple text→audio streaming interface. The returned Stream emits
// [KindAudio] frames as chunks arrive from the synthesis engine.
//
// Built-in: provider/elevenlabs, provider/googlespeech.
// Custom: implement this interface or use [TTSFunc].
type TTSProvider interface {
	Synthesize(ctx context.Context, req *TTSRequest) (*Stream, error)
}

// TTSFunc adapts a plain function to the [TTSProvider] interface.
type TTSFunc func(ctx context.Context, req *TTSRequest) (*Stream, error)

func (f TTSFunc) Synthesize(ctx context.Context, req *TTSRequest) (*Stream, error) {
	return f(ctx, req)
}

// TTSRequest contains everything needed for a text-to-speech call.
type TTSRequest struct {
	// Text is the input to synthesize.
	Text string

	// Voice selects the synthesis voice (provider-specific).
	// If empty, the provider's default voice is used.
	Voice string

	// Model selects the TTS model (provider-specific).
	// If empty, the provider's default model is used.
	Model string

	// Language is a BCP-47 language tag (e.g. "en", "es", "de").
	// Not all providers require this.
	Language string

	// OutputFormat is the desired output MIME type (e.g. AudioOGGOpus, AudioMP3).
	// If empty, the provider picks its default format.
	OutputFormat string

	// Speed is a playback speed multiplier. 1.0 = normal.
	// Not all providers support this.
	Speed float64

	// Extra carries provider-specific configuration.
	// Each provider documents its accepted types.
	Extra any
}

// ── STT ─────────────────────────────────────────────────────────────────────

// STTProvider transcribes audio to text.
//
// The returned Stream emits [KindText] frames for transcript segments.
// Interim (partial) results and final results are both KindText; use
// [STTMeta] in Frame.Extra or provider conventions to distinguish them.
//
// Built-in: provider/elevenlabs, provider/googlespeech.
// Custom: implement this interface or use [STTFunc].
type STTProvider interface {
	Transcribe(ctx context.Context, req *STTRequest) (*Stream, error)
}

// STTFunc adapts a plain function to the [STTProvider] interface.
type STTFunc func(ctx context.Context, req *STTRequest) (*Stream, error)

func (f STTFunc) Transcribe(ctx context.Context, req *STTRequest) (*Stream, error) {
	return f(ctx, req)
}

// STTRequest contains everything needed for a speech-to-text call.
type STTRequest struct {
	// Audio is the input audio data.
	// For single-shot transcription: provide the full audio bytes.
	// For streaming: use AudioStream instead.
	Audio []byte

	// AudioStream is a Stream of [KindAudio] frames for streaming transcription.
	// When set, Audio is ignored.
	AudioStream *Stream

	// InputFormat is the MIME type of the input audio (e.g. AudioPCM16k, AudioOGGOpus).
	// Required so the provider knows how to decode.
	InputFormat string

	// Model selects the STT model (provider-specific).
	// If empty, the provider's default model is used.
	Model string

	// Language is a BCP-47 language tag hint.
	Language string

	// InterimResults requests partial/interim transcripts in addition to final ones.
	InterimResults bool

	// Extra carries provider-specific configuration.
	Extra any
}

// ── Encoded audio MIME constants ────────────────────────────────────────────
//
// These complement the raw PCM constants in frame.go.
// Use them for TTS output and STT input when dealing with compressed formats.
const (
	// AudioOGGOpus is OGG-encapsulated Opus — ElevenLabs default, web-friendly.
	AudioOGGOpus = "audio/ogg;codecs=opus"

	// AudioMP3 is MPEG Layer 3 — universal playback support.
	AudioMP3 = "audio/mpeg"

	// AudioAAC is AAC in MP4 container — mobile-friendly.
	AudioAAC = "audio/aac"

	// AudioFLAC is lossless FLAC — high-quality archival.
	AudioFLAC = "audio/flac"

	// AudioWAV is RIFF/WAV — uncompressed, widely supported.
	AudioWAV = "audio/wav"

	// AudioPCMU8k is G.711 μ-law at 8kHz (telephony/PSTN).
	AudioPCMU8k = "audio/pcmu;rate=8000"

	// AudioPCMA8k is G.711 A-law at 8kHz (telephony/PSTN).
	AudioPCMA8k = "audio/pcma;rate=8000"
)
