// Package turn is a local end-of-turn detector based on silence.
//
// It does not load a model. Install it only when silence is enough to
// decide that the speaker has finished:
//
//	go get github.com/alexedtionweb/niro-stream/provider/turn
//
// [Silence.Predict] sets [niro.TurnScore.End] to silence/After, and to 1
// once Silence reaches After. A model-backed detector lives in provider/audiomodel.
package turn

import (
	"context"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

// Silence implements [niro.TurnDetector] from [niro.TurnInput.Silence].
type Silence struct {
	// After is the quiet time that means the turn is finished.
	// Zero uses 400ms.
	After time.Duration
}

func (s Silence) Predict(_ context.Context, in niro.TurnInput) (niro.TurnScore, error) {
	after := s.After
	if after <= 0 {
		after = 400 * time.Millisecond
	}
	if in.Silence <= 0 {
		return niro.TurnScore{}, nil
	}
	if in.Silence >= after {
		return niro.TurnScore{End: 1}, nil
	}
	return niro.TurnScore{End: float32(in.Silence) / float32(after)}, nil
}

var _ niro.TurnDetector = Silence{}
