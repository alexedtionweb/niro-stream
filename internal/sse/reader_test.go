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
