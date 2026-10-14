package energy

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

func TestSpeechBoundaries(t *testing.T) {
	d := New(Config{Threshold: 500, Silence: 400 * time.Millisecond, Rate: 16000})
	ctx := context.Background()
	if marks, err := d.Push(ctx, pcm(160, 0)); err != nil || len(marks) != 0 {
		t.Fatalf("quiet %v %v", marks, err)
	}
	marks, err := d.Push(ctx, pcm(160, 2000))
	if err != nil || len(marks) != 1 || marks[0].Kind != niro.SpeechStart {
		t.Fatalf("start %v %v", marks, err)
	}
	if marks, err = d.Push(ctx, pcm(1600, 0)); err != nil || len(marks) != 0 {
		t.Fatalf("short gap %v %v", marks, err)
	}
	marks, err = d.Push(ctx, pcm(8000, 0))
	if err != nil || len(marks) != 1 || marks[0].Kind != niro.SpeechEnd {
		t.Fatalf("end %v %v", marks, err)
	}
	d.Reset()
	marks, err = d.Push(ctx, pcm(160, 2000))
	if err != nil || len(marks) != 1 || marks[0].Kind != niro.SpeechStart {
		t.Fatalf("after reset %v %v", marks, err)
	}
}

func pcm(samples, amp int) []byte {
	b := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(amp)))
	}
	return b
}
