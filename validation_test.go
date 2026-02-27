package ryn_test

import (
	"encoding/json"
	"strings"
	"testing"

	"ryn.dev/ryn"
)

func TestRequestValidation(t *testing.T) {
	t.Parallel()

	t.Run("ValidRequest", func(t *testing.T) {
		req := &ryn.Request{
			Messages: []ryn.Message{ryn.UserText("hello")},
		}
		err := req.Validate()
		assertTrue(t, err == nil)
	})

	t.Run("NoMessages", func(t *testing.T) {
		req := &ryn.Request{}
		err := req.Validate()
		assertTrue(t, err != nil)
		assertTrue(t, strings.Contains(err.Message, "no messages"))
	})

	t.Run("InvalidResponseFormat", func(t *testing.T) {
		req := &ryn.Request{
			Messages:       []ryn.Message{ryn.UserText("hi")},
			ResponseFormat: "invalid_format",
		}
		err := req.Validate()
		assertTrue(t, err != nil)
	})

	t.Run("JsonSchemaMissingSchema", func(t *testing.T) {
		req := &ryn.Request{
			Messages:       []ryn.Message{ryn.UserText("hi")},
			ResponseFormat: "json_schema",
			ResponseSchema: nil,
		}
		err := req.Validate()
		assertTrue(t, err != nil)
		assertTrue(t, strings.Contains(err.Message, "ResponseSchema"))
	})

	t.Run("InvalidJSON Schema", func(t *testing.T) {
		req := &ryn.Request{
			Messages:       []ryn.Message{ryn.UserText("hi")},
			ResponseFormat: "json_schema",
			ResponseSchema: json.RawMessage(`{invalid}`),
		}
		err := req.Validate()
		assertTrue(t, err != nil)
		if err != nil {
			assertEqual(t, err.Code, ryn.ErrCodeInvalidSchema)
		}
	})

	t.Run("InvalidOptions", func(t *testing.T) {
		req := &ryn.Request{
			Messages: []ryn.Message{ryn.UserText("hi")},
			Options: ryn.Options{
				Temperature: ryn.Temp(3.0), // out of range
			},
		}
		err := req.Validate()
		assertTrue(t, err != nil)
		assertTrue(t, strings.Contains(err.Message, "Temperature"))
	})
}

func TestMessageValidation(t *testing.T) {
	t.Parallel()

	t.Run("ValidMessage", func(t *testing.T) {
		msg := ryn.UserText("hello")
		err := msg.Validate()
		assertNil(t, err)
	})

	t.Run("EmptyMessage", func(t *testing.T) {
		msg := ryn.Message{Role: ryn.RoleUser, Parts: []ryn.Part{}}
		err := msg.Validate()
		assertNotNil(t, err)
	})
}

func TestToolValidation(t *testing.T) {
	t.Parallel()

	t.Run("ValidTool", func(t *testing.T) {
		tool := ryn.Tool{
			Name:        "weather",
			Description: "Get weather",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}
		err := tool.Validate()
		assertNil(t, err)
	})

	t.Run("MissingName", func(t *testing.T) {
		tool := ryn.Tool{Description: "Get weather"}
		err := tool.Validate()
		assertNotNil(t, err)
		assertTrue(t, strings.Contains(err.Error(), "Name"))
	})
}
