package structured

import (
	"context"
	"encoding/json"

	"github.com/alexedtionweb/niro-stream"
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
	src        *niro.Stream
	buf        []byte
	partial    T
	final      T
	event      StructuredEvent[T]
	err        error
	emittedEnd bool
	resp       *niro.ResponseMeta
	usage      niro.Usage
}

// WithSchema returns a shallow copy of req configured for JSON schema output.
func WithSchema(req *niro.Request, schema json.RawMessage) *niro.Request {
	r := *req
	r.ResponseFormat = "json_schema"
	r.ResponseSchema = schema
	return &r
}

// WithSchemaAny marshals schema using the configured JSON library and applies it.
func WithSchemaAny(req *niro.Request, schema any) (*niro.Request, error) {
	b, err := niro.JSONMarshal(schema)
	if err != nil {
		return nil, err
	}
	return WithSchema(req, b), nil
}

// GenerateStructured runs a request with JSON schema output and returns the final typed result.
func GenerateStructured[T any](ctx context.Context, p niro.Provider, req *niro.Request, schema json.RawMessage) (T, *niro.ResponseMeta, niro.Usage, error) {
	var zero T

	s, err := p.Generate(ctx, WithSchema(req, schema))
	if err != nil {
		return zero, nil, niro.Usage{}, err
	}

	buf, err := collectTextBytes(ctx, s)
	if err != nil {
		return zero, nil, niro.Usage{}, err
	}
	if len(buf) == 0 {
		return zero, s.Response(), s.Usage(), niro.ErrNoStructuredOutput
	}

	var out T
	if err := niro.JSONUnmarshal(buf, &out); err != nil {
		return zero, s.Response(), s.Usage(), err
	}
	return out, s.Response(), s.Usage(), nil
}

// StreamStructured runs a request with JSON schema output and returns a decoder stream
// that yields partial and final structured outputs.
func StreamStructured[T any](ctx context.Context, p niro.Provider, req *niro.Request, schema json.RawMessage) (*StructuredStream[T], error) {
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
		if f.Kind != niro.KindText || f.Text == "" {
			continue
		}
		s.buf = append(s.buf, f.Text...)
		if niro.JSONValid(s.buf) {
			if err := niro.JSONUnmarshal(s.buf, &s.partial); err == nil {
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
		s.err = niro.ErrNoStructuredOutput
		s.emittedEnd = true
		return false
	}
	if err := niro.JSONUnmarshal(s.buf, &s.final); err != nil {
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
func (s *StructuredStream[T]) Usage() niro.Usage { return s.usage }

// Response returns provider response metadata.
func (s *StructuredStream[T]) Response() *niro.ResponseMeta { return s.resp }

func collectTextBytes(ctx context.Context, s *niro.Stream) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	for s.Next(ctx) {
		f := s.Frame()
		if f.Kind == niro.KindText {
			buf = append(buf, f.Text...)
		}
	}
	return buf, s.Err()
}
