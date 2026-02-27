package orchestrate_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"ryn.dev/ryn"
	"ryn.dev/ryn/orchestrate"
)

func TestFan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stream := orchestrate.Fan(ctx,
		func(ctx context.Context) (*ryn.Stream, error) {
			return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("A")}), nil
		},
		func(ctx context.Context) (*ryn.Stream, error) {
			return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("B")}), nil
		},
	)

	frames, err := ryn.Collect(ctx, stream)
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
		func(ctx context.Context) (*ryn.Stream, error) {
			time.Sleep(50 * time.Millisecond) // slow
			s, e := ryn.NewStream(4)
			go func() {
				defer e.Close()
				e.Emit(ctx, ryn.TextFrame("slow"))
				e.Emit(ctx, ryn.UsageFrame(&ryn.Usage{TotalTokens: 10}))
			}()
			return s, nil
		},
		func(ctx context.Context) (*ryn.Stream, error) {
			s, e := ryn.NewStream(4) // fast
			go func() {
				defer e.Close()
				e.Emit(ctx, ryn.TextFrame("fast"))
				e.Emit(ctx, ryn.UsageFrame(&ryn.Usage{TotalTokens: 5}))
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
		func(ctx context.Context, input string) (*ryn.Stream, error) {
			s, e := ryn.NewStream(2)
			go func() {
				defer e.Close()
				e.Emit(ctx, ryn.TextFrame("step1"))
			}()
			return s, nil
		},
		func(ctx context.Context, input string) (*ryn.Stream, error) {
			s, e := ryn.NewStream(2)
			go func() {
				defer e.Close()
				e.Emit(ctx, ryn.TextFrame(input+"+step2"))
			}()
			return s, nil
		},
	)

	assertNoError(t, err)
	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "step1+step2")
}

func TestSequenceEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stream, err := orchestrate.Sequence(ctx)
	assertNoError(t, err)

	frames, err := ryn.Collect(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, len(frames), 0)
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
