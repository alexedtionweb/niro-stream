package niro

import (
	"context"
	"testing"
)

func TestSpeechKindAndAdapters(t *testing.T) {
	if SpeechStart.String() != "speech_start" || SpeechEnd.String() != "speech_end" || SpeechNone.String() != "none" {
		t.Fatal(SpeechStart, SpeechEnd, SpeechNone)
	}
	var vad VAD = VADFunc(func(context.Context, []byte) ([]SpeechMark, error) {
		return []SpeechMark{{Kind: SpeechStart}}, nil
	})
	marks, err := vad.Push(context.Background(), []byte{1, 2})
	if err != nil || len(marks) != 1 || marks[0].Kind != SpeechStart {
		t.Fatalf("%v %v", marks, err)
	}
	vad.Reset()

	var turn TurnDetector = TurnFunc(func(context.Context, TurnInput) (TurnScore, error) {
		return TurnScore{End: 1}, nil
	})
	score, err := turn.Predict(context.Background(), TurnInput{Text: "done"})
	if err != nil || score.End != 1 {
		t.Fatalf("%+v %v", score, err)
	}
}
