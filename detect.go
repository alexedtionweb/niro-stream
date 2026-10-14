package niro

import (
	"context"
	"time"
)

// SpeechKind is a boundary reported by a [VAD] or derived by the caller
// from a [TurnDetector] score.
type SpeechKind uint8

const (
	SpeechNone SpeechKind = iota
	// SpeechStart is the first chunk a VAD classifies as speech.
	SpeechStart
	// SpeechEnd is the chunk where speech has been quiet long enough to close.
	SpeechEnd
)

func (k SpeechKind) String() string {
	switch k {
	case SpeechStart:
		return "speech_start"
	case SpeechEnd:
		return "speech_end"
	default:
		return "none"
	}
}

// SpeechMark is one boundary inside a PCM stream.
type SpeechMark struct {
	Kind SpeechKind
	// At is the detector's clock for this mark. Energy VAD sets it to how
	// long the current speech or silence run has lasted.
	At time.Duration
	// Probability is set by a model-backed VAD. The energy detector leaves it 0.
	Probability float32
}

// VAD reports speech boundaries from successive PCM16 mono chunks.
// Push is called in order on one stream. Reset starts a new stream.
//
// The core module only defines this interface. Implementations are
// separate modules: provider/energy (local) and provider/audiomodel (HTTP model).
type VAD interface {
	Push(ctx context.Context, pcm []byte) ([]SpeechMark, error)
	Reset()
}

// VADFunc adapts a function to [VAD]. Reset is a no-op.
type VADFunc func(ctx context.Context, pcm []byte) ([]SpeechMark, error)

func (f VADFunc) Push(ctx context.Context, pcm []byte) ([]SpeechMark, error) {
	if f == nil {
		return nil, nil
	}
	return f(ctx, pcm)
}

func (VADFunc) Reset() {}

// TurnInput is the evidence a turn detector may use. A detector reads
// the fields it understands and ignores the rest.
type TurnInput struct {
	// Text is the transcript of the current user turn, when one exists.
	Text string
	// Silence is how long the speaker has been quiet.
	Silence time.Duration
	// PCM is recent audio. Empty when the detector is text-only.
	PCM []byte
	// MIME describes PCM. Empty means the detector's default rate.
	MIME string
}

// TurnScore is a turn detector's decision.
// End is the probability in [0, 1] that the speaker has finished.
// The caller chooses the cutoff; this package does not.
type TurnScore struct {
	End float32
}

// TurnDetector scores whether the current speaker turn is finished.
//
// Implementations are separate modules: provider/turn (local silence) and
// provider/audiomodel (HTTP model).
type TurnDetector interface {
	Predict(ctx context.Context, in TurnInput) (TurnScore, error)
}

// TurnFunc adapts a function to [TurnDetector].
type TurnFunc func(ctx context.Context, in TurnInput) (TurnScore, error)

func (f TurnFunc) Predict(ctx context.Context, in TurnInput) (TurnScore, error) {
	if f == nil {
		return TurnScore{}, nil
	}
	return f(ctx, in)
}
