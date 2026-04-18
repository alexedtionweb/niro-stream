package sse_test

import (
	"io"
	"strings"
	"testing"

	"github.com/alexedtionweb/niro-stream/internal/sse"
)

func TestReaderBasic(t *testing.T) {
	t.Parallel()

	input := "data: hello\n\ndata: world\n\ndata: [DONE]\n\n"
	r := sse.NewReader(strings.NewReader(input))

	ev, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(ev.Data) != "hello" {
		t.Errorf("got %q, want %q", ev.Data, "hello")
	}

	ev, err = r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(ev.Data) != "world" {
		t.Errorf("got %q, want %q", ev.Data, "world")
	}

	_, err = r.Next()
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestReaderSkipsNonData(t *testing.T) {
	t.Parallel()

	input := ": comment\nevent: something\nid: 1\nretry: 5000\ndata: payload\n\n"
	r := sse.NewReader(strings.NewReader(input))

	ev, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(ev.Data) != "payload" {
		t.Errorf("got %q, want %q", ev.Data, "payload")
	}
}

func TestReaderEmptyStream(t *testing.T) {
	t.Parallel()

	r := sse.NewReader(strings.NewReader(""))
	_, err := r.Next()
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestReaderDataWithSpace(t *testing.T) {
	t.Parallel()

	input := "data: {\"key\": \"value\"}\n\n"
	r := sse.NewReader(strings.NewReader(input))

	ev, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(ev.Data) != `{"key": "value"}` {
		t.Errorf("got %q", ev.Data)
	}
}

// TestReaderLargeLine exercises the regression that caused providers
// emitting > 64 KiB SSE chunks (e.g. fully-accumulated tool-call arguments
// or large structured-output JSON) to fail with bufio.ErrTooLong.
func TestReaderLargeLine(t *testing.T) {
	t.Parallel()

	const payloadSize = 256 << 10 // 256 KiB — well above bufio.Scanner's default 64 KiB
	big := strings.Repeat("x", payloadSize)
	input := "data: " + big + "\n\ndata: [DONE]\n\n"

	r := sse.NewReader(strings.NewReader(input))
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("expected success on >64KiB SSE line, got %v", err)
	}
	if len(ev.Data) != payloadSize {
		t.Errorf("payload truncated: got %d bytes, want %d", len(ev.Data), payloadSize)
	}

	if _, err := r.Next(); err != io.EOF {
		t.Errorf("expected EOF after [DONE], got %v", err)
	}
}
