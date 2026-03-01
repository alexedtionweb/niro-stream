package orchestrate_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/orchestrate"
)

func TestFan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stream := orchestrate.Fan(ctx,
		func(ctx context.Context) (*niro.Stream, error) {
			return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("A")}), nil
		},
		func(ctx context.Context) (*niro.Stream, error) {
			return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("B")}), nil
		},
	)

	frames, err := niro.Collect(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, len(frames), 2)

	texts := map[string]bool{}
	for _, f := range frames {
		texts[f.Text] = true
	}
	assertTrue(t, texts["A"])
	assertTrue(t, texts["B"])
}

func TestRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	text, usage, err := orchestrate.Race(ctx,
		func(ctx context.Context) (*niro.Stream, error) {
			time.Sleep(50 * time.Millisecond) // slow
			s, e := niro.NewStream(4)
			go func() {
				defer e.Close()
				e.Emit(ctx, niro.TextFrame("slow"))
				e.Emit(ctx, niro.UsageFrame(&niro.Usage{TotalTokens: 10}))
			}()
			return s, nil
		},
		func(ctx context.Context) (*niro.Stream, error) {
			s, e := niro.NewStream(4) // fast
			go func() {
				defer e.Close()
				e.Emit(ctx, niro.TextFrame("fast"))
				e.Emit(ctx, niro.UsageFrame(&niro.Usage{TotalTokens: 5}))
			}()
			return s, nil
		},
	)

	assertNoError(t, err)
	assertEqual(t, text, "fast")
	assertEqual(t, usage.TotalTokens, 5)
}

func TestSequence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stream, err := orchestrate.Sequence(ctx,
		func(ctx context.Context, input string) (*niro.Stream, error) {
			s, e := niro.NewStream(2)
			go func() {
				defer e.Close()
				e.Emit(ctx, niro.TextFrame("step1"))
			}()
			return s, nil
		},
		func(ctx context.Context, input string) (*niro.Stream, error) {
			s, e := niro.NewStream(2)
			go func() {
				defer e.Close()
				e.Emit(ctx, niro.TextFrame(input+"+step2"))
			}()
			return s, nil
		},
	)

	assertNoError(t, err)
	text, err := niro.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "step1+step2")
}

func TestSequenceEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stream, err := orchestrate.Sequence(ctx)
	assertNoError(t, err)

	frames, err := niro.Collect(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, len(frames), 0)
}

func TestFanWithError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stream := orchestrate.Fan(ctx,
		func(ctx context.Context) (*niro.Stream, error) {
			return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
		},
		func(ctx context.Context) (*niro.Stream, error) {
			return nil, fmt.Errorf("fn failed")
		},
	)

	// consume; the error should be set
	var textSeen bool
	for stream.Next(ctx) {
		f := stream.Frame()
		if f.Kind == niro.KindText {
			textSeen = true
		}
	}
	// error should be propagated
	assertTrue(t, stream.Err() != nil)
	assertTrue(t, textSeen) // the successful stream still delivered
}

func TestFanStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stream := orchestrate.Fan(ctx,
		func(ctx context.Context) (*niro.Stream, error) {
			s, e := niro.NewStream(4)
			go func() {
				defer e.Close()
				e.Error(fmt.Errorf("stream internal error"))
			}()
			return s, nil
		},
	)

	for stream.Next(ctx) {
	}
	assertTrue(t, stream.Err() != nil)
}

func TestFanEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stream := orchestrate.Fan(ctx) // no functions
	frames, err := niro.Collect(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, len(frames), 0)
}

func TestRaceAllError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, _, err := orchestrate.Race(ctx,
		func(ctx context.Context) (*niro.Stream, error) {
			return nil, fmt.Errorf("fn1 failed")
		},
		func(ctx context.Context) (*niro.Stream, error) {
			return nil, fmt.Errorf("fn2 failed")
		},
	)
	assertTrue(t, err != nil)
}

func TestRaceStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, _, err := orchestrate.Race(ctx,
		func(ctx context.Context) (*niro.Stream, error) {
			s, e := niro.NewStream(4)
			go func() {
				defer e.Close()
				e.Error(fmt.Errorf("stream error"))
			}()
			return s, nil
		},
	)
	assertTrue(t, err != nil)
}

func TestSequenceStepError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, err := orchestrate.Sequence(ctx,
		func(ctx context.Context, input string) (*niro.Stream, error) {
			return nil, fmt.Errorf("step failed")
		},
		func(ctx context.Context, input string) (*niro.Stream, error) {
			return niro.StreamFromSlice(nil), nil
		},
	)
	assertTrue(t, err != nil)
}

func TestSequenceIntermediateStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, err := orchestrate.Sequence(ctx,
		func(ctx context.Context, input string) (*niro.Stream, error) {
			s, e := niro.NewStream(4)
			go func() {
				defer e.Close()
				e.Error(fmt.Errorf("intermediate error"))
			}()
			return s, nil
		},
		func(ctx context.Context, input string) (*niro.Stream, error) {
			return niro.StreamFromSlice(nil), nil
		},
	)
	assertTrue(t, err != nil)
}

func TestSequenceSingleStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stream, err := orchestrate.Sequence(ctx,
		func(ctx context.Context, input string) (*niro.Stream, error) {
			return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("only")}), nil
		},
	)
	assertNoError(t, err)
	text, err := niro.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "only")
}

// --- test helpers ---

func assertEqual[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func assertTrue(t *testing.T, v bool) {
	t.Helper()
	if !v {
		t.Error("expected true")
	}
}

func strContains(s, sub string) bool {
	return strings.Contains(s, sub)
}

var _ = strContains // avoid unused
