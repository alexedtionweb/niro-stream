// Package sse provides a minimal Server-Sent Events reader.
// It handles the SSE wire format used by OpenAI, Anthropic,
// and most streaming LLM APIs.
//
// This is an internal package — not part of the public API.
package sse

import (
	"bufio"
	"bytes"
	"io"
)

// Event represents a single Server-Sent Event.
type Event struct {
	Data []byte
}

// Reader reads Server-Sent Events from an io.Reader.
type Reader struct {
	sc *bufio.Scanner
}

// NewReader creates an SSE Reader.
func NewReader(r io.Reader) *Reader {
	return &Reader{sc: bufio.NewScanner(r)}
}

var (
	dataPrefix = []byte("data:")
	doneMarker = []byte("[DONE]")
	space      = []byte(" ")
)

// Next reads the next SSE data event.
// Skips non-data lines (event:, id:, retry:, comments, blank lines).
// Returns io.EOF when the stream ends or a [DONE] marker is received.
func (r *Reader) Next() (Event, error) {
	for r.sc.Scan() {
		line := r.sc.Bytes()
		if !bytes.HasPrefix(line, dataPrefix) {
			continue
		}
		payload := bytes.TrimPrefix(line, dataPrefix)
		payload = bytes.TrimPrefix(payload, space)

		if bytes.Equal(payload, doneMarker) {
			return Event{}, io.EOF
		}
		return Event{Data: bytes.Clone(payload)}, nil
	}
	if err := r.sc.Err(); err != nil {
		return Event{}, err
	}
	return Event{}, io.EOF
}
