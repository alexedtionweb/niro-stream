package ryn

import (
	"context"
	"encoding/json"
)

// StructuredEvent is emitted by StructuredStream.Next.
//
// Partial is set when a valid JSON object/array can be parsed from the stream
// before completion. Final is set once at the end of the stream.
//
// The pointers are only valid until the next call to Next.
type StructuredEvent[T any] struct {
	Partial *T
	Final   *T
	Done    bool
}

// StructuredStream decodes a stream of text frames into typed JSON output.
type StructuredStream[T any] struct {
	src        *Stream
	buf        []byte
	partial    T
	final      T
	event      StructuredEvent[T]
	err        error
	emittedEnd bool
	resp       *ResponseMeta
	usage      Usage
}

// WithSchema returns a shallow copy of req configured for JSON schema output.
func WithSchema(req *Request, schema json.RawMessage) *Request {
	r := *req
	r.ResponseFormat = "json_schema"
	r.ResponseSchema = schema
	return &r
}

// WithSchemaAny marshals schema using the configured JSON library and applies it.
func WithSchemaAny(req *Request, schema any) (*Request, error) {
	b, err := JSONMarshal(schema)
	if err != nil {
		return nil, err
	}
	return WithSchema(req, b), nil
}

// GenerateStructured runs a request with JSON schema output and returns the final typed result.
func GenerateStructured[T any](ctx context.Context, p Provider, req *Request, schema json.RawMessage) (T, *ResponseMeta, Usage, error) {
	var zero T

	s, err := p.Generate(ctx, WithSchema(req, schema))
	if err != nil {
		return zero, nil, Usage{}, err
	}

	buf, err := collectTextBytes(ctx, s)
	if err != nil {
		return zero, nil, Usage{}, err
	}
	if len(buf) == 0 {
		return zero, s.Response(), s.Usage(), ErrNoStructuredOutput
	}

	var out T
	if err := JSONUnmarshal(buf, &out); err != nil {
		return zero, s.Response(), s.Usage(), err
	}
	return out, s.Response(), s.Usage(), nil
}

// StreamStructured runs a request with JSON schema output and returns a decoder stream
// that yields partial and final structured outputs.
func StreamStructured[T any](ctx context.Context, p Provider, req *Request, schema json.RawMessage) (*StructuredStream[T], error) {
	s, err := p.Generate(ctx, WithSchema(req, schema))
	if err != nil {
		return nil, err
	}
	st := &StructuredStream[T]{
		src: s,
		buf: make([]byte, 0, 4096),
	}
	return st, nil
}

// Next advances the structured stream and emits either a partial or final event.
func (s *StructuredStream[T]) Next(ctx context.Context) bool {
	if s.emittedEnd {
		return false
	}

	for s.src.Next(ctx) {
		f := s.src.Frame()
		if f.Kind != KindText || f.Text == "" {
			continue
		}
		s.buf = append(s.buf, f.Text...)
		if JSONValid(s.buf) {
			if err := JSONUnmarshal(s.buf, &s.partial); err == nil {
				s.event = StructuredEvent[T]{Partial: &s.partial}
				return true
			}
		}
	}

	if err := s.src.Err(); err != nil {
		s.err = err
		s.emittedEnd = true
		return false
	}

	// End of stream: emit final
	s.resp = s.src.Response()
	s.usage = s.src.Usage()
	if len(s.buf) == 0 {
		s.err = ErrNoStructuredOutput
		s.emittedEnd = true
		return false
	}
	if err := JSONUnmarshal(s.buf, &s.final); err != nil {
		s.err = err
		s.emittedEnd = true
		return false
	}
	s.event = StructuredEvent[T]{Final: &s.final, Done: true}
	s.emittedEnd = true
	return true
}

// Event returns the current structured event.
func (s *StructuredStream[T]) Event() StructuredEvent[T] { return s.event }

// Err returns the first error encountered during decoding.
func (s *StructuredStream[T]) Err() error { return s.err }

// Usage returns the accumulated token usage.
func (s *StructuredStream[T]) Usage() Usage { return s.usage }

// Response returns provider response metadata.
func (s *StructuredStream[T]) Response() *ResponseMeta { return s.resp }

func collectTextBytes(ctx context.Context, s *Stream) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	for s.Next(ctx) {
		f := s.Frame()
		if f.Kind == KindText {
			buf = append(buf, f.Text...)
		}
	}
	return buf, s.Err()
}
