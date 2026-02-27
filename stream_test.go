package ryn_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"ryn.dev/ryn"
)

func TestStreamBasic(t *testing.T) {
	ctx := context.Background()
	s, e := ryn.NewStream(0)

	go func() {
		defer e.Close()
		e.Emit(ctx, ryn.TextFrame("hello"))
		e.Emit(ctx, ryn.TextFrame(" "))
		e.Emit(ctx, ryn.TextFrame("world"))
	}()

	got, err := ryn.CollectText(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestStreamBuffered(t *testing.T) {
	ctx := context.Background()
	s, e := ryn.NewStream(8)

	// Write all before reading — buffered channels allow this
	for i := range 5 {
		if err := e.Emit(ctx, ryn.TextFrame(fmt.Sprintf("%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	e.Close()

	got, err := ryn.CollectText(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if got != "01234" {
		t.Errorf("got %q, want %q", got, "01234")
	}
}

func TestStreamFromSlice(t *testing.T) {
	ctx := context.Background()
	frames := []ryn.Frame{
		ryn.TextFrame("a"),
		ryn.TextFrame("b"),
		ryn.TextFrame("c"),
	}
	s := ryn.StreamFromSlice(frames)

	got, err := ryn.Collect(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d frames, want 3", len(got))
	}
	for i, f := range got {
		want := string(rune('a' + i))
		if f.Text != want {
			t.Errorf("frame %d: got %q, want %q", i, f.Text, want)
		}
	}
}

func TestStreamCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s, _ := ryn.NewStream(0)

	cancel()
	if s.Next(ctx) {
		t.Error("Next should return false on canceled context")
	}
	if s.Err() != context.Canceled {
		t.Errorf("got %v, want context.Canceled", s.Err())
	}
}

func TestStreamError(t *testing.T) {
	ctx := context.Background()
	s, e := ryn.NewStream(1) // buffered so Emit doesn't block

	go func() {
		e.Emit(ctx, ryn.TextFrame("hello"))
		e.Error(fmt.Errorf("something broke"))
	}()

	// Should get the first frame
	if !s.Next(ctx) {
		t.Fatal("expected first frame")
	}
	if s.Frame().Text != "hello" {
		t.Errorf("got %q, want %q", s.Frame().Text, "hello")
	}

	// Should see the error
	if s.Next(ctx) {
		t.Error("expected Next to return false after error")
	}
	if s.Err() == nil || s.Err().Error() != "something broke" {
		t.Errorf("got %v, want 'something broke'", s.Err())
	}
}

func TestStreamMultimodal(t *testing.T) {
	ctx := context.Background()
	frames := []ryn.Frame{
		ryn.TextFrame("describe this:"),
		ryn.ImageFrame([]byte{0x89, 0x50, 0x4E, 0x47}, "image/png"),
		ryn.AudioFrame([]byte{0x00, 0x01}, "audio/pcm"),
	}
	s := ryn.StreamFromSlice(frames)

	got, err := ryn.Collect(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d frames, want 3", len(got))
	}
	if got[0].Kind != ryn.KindText {
		t.Errorf("frame 0: got kind %v, want text", got[0].Kind)
	}
	if got[1].Kind != ryn.KindImage {
		t.Errorf("frame 1: got kind %v, want image", got[1].Kind)
	}
	if got[2].Kind != ryn.KindAudio {
		t.Errorf("frame 2: got kind %v, want audio", got[2].Kind)
	}
}

func TestEmitAfterClose(t *testing.T) {
	ctx := context.Background()
	_, e := ryn.NewStream(0)
	e.Close()

	err := e.Emit(ctx, ryn.TextFrame("late"))
	if err != ryn.ErrClosed {
		t.Errorf("got %v, want ErrClosed", err)
	}
}

func TestForward(t *testing.T) {
	ctx := context.Background()
	src := ryn.StreamFromSlice([]ryn.Frame{
		ryn.TextFrame("a"),
		ryn.TextFrame("b"),
	})

	dst, dstEmit := ryn.NewStream(4)
	go func() {
		defer dstEmit.Close()
		ryn.Forward(ctx, src, dstEmit)
	}()

	got, err := ryn.CollectText(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ab" {
		t.Errorf("got %q, want %q", got, "ab")
	}
}

func TestStreamBackpressure(t *testing.T) {
	// Unbuffered stream: writer should block until reader consumes
	ctx := context.Background()
	s, e := ryn.NewStream(0)

	emitted := make(chan struct{})
	go func() {
		e.Emit(ctx, ryn.TextFrame("blocked"))
		close(emitted)
		e.Close()
	}()

	// Give the goroutine time to attempt emit
	select {
	case <-emitted:
		t.Fatal("Emit should block on unbuffered stream")
	case <-time.After(50 * time.Millisecond):
		// expected — writer is blocked
	}

	// Now consume
	if !s.Next(ctx) {
		t.Fatal("expected frame")
	}

	// Writer should now be unblocked
	select {
	case <-emitted:
		// good
	case <-time.After(time.Second):
		t.Fatal("writer still blocked after read")
	}
}
