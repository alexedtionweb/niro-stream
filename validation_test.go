package ryn_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexedtionweb/niro-stream"
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

	t.Run("NilRequest", func(t *testing.T) {
		var req *ryn.Request
		err := req.Validate()
		assertNotNil(t, err)
	})

	t.Run("NoMessages", func(t *testing.T) {
		req := &ryn.Request{}
		err := req.Validate()
		assertTrue(t, err != nil)
		assertTrue(t, strings.Contains(err.Message, "no messages"))
	})

	t.Run("SystemPromptOnly", func(t *testing.T) {
		req := &ryn.Request{SystemPrompt: "be helpful"}
		err := req.Validate()
		assertNil(t, err)
	})

	t.Run("InvalidResponseFormat", func(t *testing.T) {
		req := &ryn.Request{
			Messages:       []ryn.Message{ryn.UserText("hi")},
			ResponseFormat: "invalid_format",
		}
		err := req.Validate()
		assertTrue(t, err != nil)
	})

	t.Run("ValidResponseFormats", func(t *testing.T) {
		for _, fmt := range []string{"json", "text", "json_schema"} {
			schema := json.RawMessage(`{}`)
			req := &ryn.Request{
				Messages:       []ryn.Message{ryn.UserText("hi")},
				ResponseFormat: fmt,
				ResponseSchema: schema,
			}
			if fmt != "json_schema" {
				req.ResponseSchema = nil
			}
			if fmt == "json" || fmt == "text" {
				err := req.Validate()
				assertNil(t, err)
			}
		}
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

	t.Run("InvalidJSONSchema", func(t *testing.T) {
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

	t.Run("InvalidMessage", func(t *testing.T) {
		req := &ryn.Request{
			Messages: []ryn.Message{
				{Role: ryn.RoleUser, Parts: []ryn.Part{}}, // empty parts
			},
		}
		err := req.Validate()
		assertNotNil(t, err)
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

	t.Run("InvalidPart", func(t *testing.T) {
		msg := ryn.Message{
			Role:  ryn.RoleUser,
			Parts: []ryn.Part{{Kind: ryn.KindImage}}, // image with no data or URL
		}
		err := msg.Validate()
		assertNotNil(t, err)
	})
}

func TestPartValidation(t *testing.T) {
	t.Parallel()

	t.Run("TextPart", func(t *testing.T) {
		p := ryn.TextPart("hello")
		assertNil(t, p.Validate())
	})

	t.Run("ImageWithData", func(t *testing.T) {
		p := ryn.ImagePart([]byte{0xFF}, "image/jpeg")
		assertNil(t, p.Validate())
	})

	t.Run("ImageURLOnly", func(t *testing.T) {
		p := ryn.Part{Kind: ryn.KindImage, URL: "https://example.com/img.png"}
		assertNil(t, p.Validate())
	})

	t.Run("AudioNoData", func(t *testing.T) {
		p := ryn.Part{Kind: ryn.KindAudio}
		assertNotNil(t, p.Validate())
	})

	t.Run("AudioDataNoMime", func(t *testing.T) {
		p := ryn.Part{Kind: ryn.KindAudio, Data: []byte{0x01}}
		assertNotNil(t, p.Validate())
	})

	t.Run("VideoWithData", func(t *testing.T) {
		p := ryn.VideoPart([]byte{0x00}, "video/mp4")
		assertNil(t, p.Validate())
	})

	t.Run("ToolCallNilTool", func(t *testing.T) {
		p := ryn.Part{Kind: ryn.KindToolCall, Tool: nil}
		assertNotNil(t, p.Validate())
	})

	t.Run("ToolCallValid", func(t *testing.T) {
		p := ryn.ToolCallPart(&ryn.ToolCall{ID: "c1", Name: "fn"})
		assertNil(t, p.Validate())
	})

	t.Run("ToolResultNil", func(t *testing.T) {
		p := ryn.Part{Kind: ryn.KindToolResult, Result: nil}
		assertNotNil(t, p.Validate())
	})

	t.Run("ToolResultValid", func(t *testing.T) {
		p := ryn.ToolResultPart(&ryn.ToolResult{CallID: "c1"})
		assertNil(t, p.Validate())
	})

	t.Run("UnknownKind", func(t *testing.T) {
		p := ryn.Part{Kind: ryn.Kind(99)}
		assertNotNil(t, p.Validate())
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

	t.Run("MissingDescription", func(t *testing.T) {
		tool := ryn.Tool{Name: "weather"}
		err := tool.Validate()
		assertNotNil(t, err)
		assertTrue(t, strings.Contains(err.Error(), "Description"))
	})

	t.Run("InvalidParameters", func(t *testing.T) {
		tool := ryn.Tool{
			Name:        "weather",
			Description: "Get weather",
			Parameters:  json.RawMessage(`{invalid}`),
		}
		err := tool.Validate()
		assertNotNil(t, err)
	})
}

func TestToolCallValidation(t *testing.T) {
	t.Parallel()

	t.Run("Valid", func(t *testing.T) {
		tc := &ryn.ToolCall{ID: "c1", Name: "fn", Args: json.RawMessage(`{}`)}
		assertNil(t, tc.Validate())
	})

	t.Run("MissingID", func(t *testing.T) {
		tc := &ryn.ToolCall{Name: "fn"}
		assertNotNil(t, tc.Validate())
	})

	t.Run("MissingName", func(t *testing.T) {
		tc := &ryn.ToolCall{ID: "c1"}
		assertNotNil(t, tc.Validate())
	})

	t.Run("InvalidArgs", func(t *testing.T) {
		tc := &ryn.ToolCall{ID: "c1", Name: "fn", Args: json.RawMessage(`{invalid}`)}
		assertNotNil(t, tc.Validate())
	})
}

func TestToolResultValidation(t *testing.T) {
	t.Parallel()

	t.Run("Valid", func(t *testing.T) {
		tr := &ryn.ToolResult{CallID: "c1", Content: "ok"}
		assertNil(t, tr.Validate())
	})

	t.Run("MissingCallID", func(t *testing.T) {
		tr := &ryn.ToolResult{Content: "ok"}
		assertNotNil(t, tr.Validate())
	})
}

func TestToolChoiceValidation(t *testing.T) {
	t.Parallel()

	t.Run("Empty", func(t *testing.T) {
		assertNil(t, ryn.ToolChoice("").Validate())
	})

	t.Run("Auto", func(t *testing.T) {
		assertNil(t, ryn.ToolChoiceAuto.Validate())
	})

	t.Run("Required", func(t *testing.T) {
		assertNil(t, ryn.ToolChoiceRequired.Validate())
	})

	t.Run("None", func(t *testing.T) {
		assertNil(t, ryn.ToolChoiceNone.Validate())
	})

	t.Run("FuncPattern", func(t *testing.T) {
		assertNil(t, ryn.ToolChoiceFunc("weather").Validate())
	})

	t.Run("Invalid", func(t *testing.T) {
		assertNotNil(t, ryn.ToolChoice("invalid_choice").Validate())
	})
}

func TestOptionsValidation(t *testing.T) {
	t.Parallel()

	t.Run("Nil", func(t *testing.T) {
		var o *ryn.Options
		assertNil(t, o.Validate())
	})

	t.Run("NegativeMaxTokens", func(t *testing.T) {
		o := &ryn.Options{MaxTokens: -1}
		assertNotNil(t, o.Validate())
	})

	t.Run("ValidTemperature", func(t *testing.T) {
		o := &ryn.Options{Temperature: ryn.Temp(0.5)}
		assertNil(t, o.Validate())
	})

	t.Run("TooHighTemperature", func(t *testing.T) {
		o := &ryn.Options{Temperature: ryn.Temp(2.5)}
		assertNotNil(t, o.Validate())
	})

	t.Run("NegativeTemperature", func(t *testing.T) {
		o := &ryn.Options{Temperature: ryn.Temp(-0.1)}
		assertNotNil(t, o.Validate())
	})

	t.Run("ValidTopP", func(t *testing.T) {
		o := &ryn.Options{TopP: ryn.TopPVal(0.9)}
		assertNil(t, o.Validate())
	})

	t.Run("TooHighTopP", func(t *testing.T) {
		o := &ryn.Options{TopP: ryn.TopPVal(1.1)}
		assertNotNil(t, o.Validate())
	})

	t.Run("NegativeTopP", func(t *testing.T) {
		o := &ryn.Options{TopP: ryn.TopPVal(-0.1)}
		assertNotNil(t, o.Validate())
	})

	t.Run("ValidTopK", func(t *testing.T) {
		o := &ryn.Options{TopK: ryn.TopKVal(40)}
		assertNil(t, o.Validate())
	})

	t.Run("NegativeTopK", func(t *testing.T) {
		o := &ryn.Options{TopK: ryn.TopKVal(-1)}
		assertNotNil(t, o.Validate())
	})

	t.Run("FrequencyPenaltyOutOfRange", func(t *testing.T) {
		fp := 3.0
		o := &ryn.Options{FrequencyPenalty: &fp}
		assertNotNil(t, o.Validate())
	})

	t.Run("PresencePenaltyOutOfRange", func(t *testing.T) {
		pp := -3.0
		o := &ryn.Options{PresencePenalty: &pp}
		assertNotNil(t, o.Validate())
	})

	t.Run("ValidPenalties", func(t *testing.T) {
		fp := 1.5
		pp := -1.5
		o := &ryn.Options{FrequencyPenalty: &fp, PresencePenalty: &pp}
		assertNil(t, o.Validate())
	})
}

func TestRequestWithToolValidation(t *testing.T) {
	t.Parallel()

	t.Run("ToolChoiceWithTools", func(t *testing.T) {
		req := &ryn.Request{
			Messages: []ryn.Message{ryn.UserText("hi")},
			Tools: []ryn.Tool{{
				Name:        "weather",
				Description: "Get weather",
			}},
			ToolChoice: ryn.ToolChoiceAuto,
		}
		err := req.Validate()
		assertNil(t, err)
	})

	t.Run("InvalidToolChoiceWithTools", func(t *testing.T) {
		req := &ryn.Request{
			Messages: []ryn.Message{ryn.UserText("hi")},
			Tools: []ryn.Tool{{
				Name:        "weather",
				Description: "Get weather",
			}},
			ToolChoice: ryn.ToolChoice("bad_choice"),
		}
		err := req.Validate()
		assertNotNil(t, err)
	})

	t.Run("InvalidTool", func(t *testing.T) {
		req := &ryn.Request{
			Messages: []ryn.Message{ryn.UserText("hi")},
			Tools:    []ryn.Tool{{Name: "weather"}}, // missing description
		}
		err := req.Validate()
		assertNotNil(t, err)
	})
}
