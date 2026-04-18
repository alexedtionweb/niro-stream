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

// MaxLineSize is the maximum SSE line length accepted by [NewReader].
//
// The default bufio.Scanner buffer (64 KiB) is too small for streaming LLM
// responses: a single SSE chunk can carry a fully-accumulated tool-call
// arguments blob or a long structured-output JSON value, which routinely
// exceeds 64 KiB. When the limit is reached, [Reader.Next] returns
// bufio.ErrTooLong and the stream is permanently broken.
//
// 4 MiB is high enough to absorb the largest payloads emitted by the
// production providers we target while still capping memory per connection.
const MaxLineSize = 4 << 20 // 4 MiB

// NewReader creates an SSE Reader that supports lines up to [MaxLineSize].
//
// The buffer grows on demand from a small initial size; the cap protects
// the process from a hostile or buggy server that never terminates a line.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	// Start small to keep idle connections cheap; allow growth up to MaxLineSize.
	sc.Buffer(make([]byte, 0, 64<<10), MaxLineSize)
	return &Reader{sc: sc}
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
