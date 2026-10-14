package turn

import (
	"context"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

func TestSilenceScore(t *testing.T) {
	s := Silence{After: 400 * time.Millisecond}
	ctx := context.Background()
	got, err := s.Predict(ctx, niro.TurnInput{})
	if err != nil || got.End != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	got, err = s.Predict(ctx, niro.TurnInput{Silence: 200 * time.Millisecond})
	if err != nil || got.End < 0.49 || got.End > 0.51 {
		t.Fatalf("%+v %v", got, err)
	}
	got, err = s.Predict(ctx, niro.TurnInput{Silence: 400 * time.Millisecond})
	if err != nil || got.End != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}
