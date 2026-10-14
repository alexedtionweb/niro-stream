// Package energy is a local PCM voice-activity detector.
//
// It has no model weights and no network calls. Install it only when you
// want this detector:
//
//	go get github.com/alexedtionweb/niro-stream/provider/energy
//
// Push successive PCM16 little-endian mono chunks. A chunk whose RMS is at
// or above Threshold emits [niro.SpeechStart]. After Silence of quieter
// chunks it emits [niro.SpeechEnd].
package energy

import (
	"context"
	"encoding/binary"
	"math"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

// Config tunes the detector. Zero values use the defaults.
type Config struct {
	// Threshold is the RMS of int16 samples that counts as speech.
	// Default 500.
	Threshold int
	// Silence is how long audio must stay under Threshold before SpeechEnd.
	// Default 400ms.
	Silence time.Duration
	// Rate is the sample rate of the PCM. Default 16000.
	Rate int
}

// Detector implements [niro.VAD].
type Detector struct {
	threshold int
	silence   time.Duration
	rate      int
	speaking  bool
	quiet     time.Duration
}

func New(cfg Config) *Detector {
	if cfg.Threshold <= 0 {
		cfg.Threshold = 500
	}
	if cfg.Silence <= 0 {
		cfg.Silence = 400 * time.Millisecond
	}
	if cfg.Rate <= 0 {
		cfg.Rate = 16000
	}
	return &Detector{threshold: cfg.Threshold, silence: cfg.Silence, rate: cfg.Rate}
}

func (d *Detector) Reset() {
	d.speaking = false
	d.quiet = 0
}

func (d *Detector) Push(_ context.Context, pcm []byte) ([]niro.SpeechMark, error) {
	n := len(pcm) / 2
	if n == 0 {
		return nil, nil
	}
	var sum int64
	for i := 0; i < n; i++ {
		v := int32(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		sum += int64(v * v)
	}
	rms := int(math.Sqrt(float64(sum) / float64(n)))
	dur := time.Duration(n) * time.Second / time.Duration(d.rate)
	if rms >= d.threshold {
		d.quiet = 0
		if d.speaking {
			return nil, nil
		}
		d.speaking = true
		return []niro.SpeechMark{{Kind: niro.SpeechStart, At: dur}}, nil
	}
	if !d.speaking {
		return nil, nil
	}
	d.quiet += dur
	if d.quiet < d.silence {
		return nil, nil
	}
	at := d.quiet
	d.speaking = false
	d.quiet = 0
	return []niro.SpeechMark{{Kind: niro.SpeechEnd, At: at}}, nil
}

var _ niro.VAD = (*Detector)(nil)
