package niro_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexedtionweb/niro-stream"
)

func TestRequestValidation(t *testing.T) {
	t.Parallel()

	t.Run("ValidRequest", func(t *testing.T) {
		req := &niro.Request{
			Messages: []niro.Message{niro.UserText("hello")},
		}
		err := req.Validate()
		assertTrue(t, err == nil)
	})

	t.Run("NilRequest", func(t *testing.T) {
		var req *niro.Request
		err := req.Validate()
		assertNotNil(t, err)
	})

	t.Run("NoMessages", func(t *testing.T) {
		req := &niro.Request{}
		err := req.Validate()
		assertTrue(t, err != nil)
		assertTrue(t, strings.Contains(err.Message, "no messages"))
	})

	t.Run("SystemPromptOnly", func(t *testing.T) {
		req := &niro.Request{SystemPrompt: "be helpful"}
		err := req.Validate()
		assertNil(t, err)
	})

	t.Run("InvalidResponseFormat", func(t *testing.T) {
		req := &niro.Request{
			Messages:       []niro.Message{niro.UserText("hi")},
			ResponseFormat: "invalid_format",
		}
		err := req.Validate()
		assertTrue(t, err != nil)
	})

	t.Run("ValidResponseFormats", func(t *testing.T) {
		for _, fmt := range []string{"json", "text", "json_schema"} {
			schema := json.RawMessage(`{}`)
			req := &niro.Request{
				Messages:       []niro.Message{niro.UserText("hi")},
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
		req := &niro.Request{
			Messages:       []niro.Message{niro.UserText("hi")},
			ResponseFormat: "json_schema",
			ResponseSchema: nil,
		}
		err := req.Validate()
		assertTrue(t, err != nil)
		assertTrue(t, strings.Contains(err.Message, "ResponseSchema"))
	})

	t.Run("InvalidJSONSchema", func(t *testing.T) {
		req := &niro.Request{
			Messages:       []niro.Message{niro.UserText("hi")},
			ResponseFormat: "json_schema",
			ResponseSchema: json.RawMessage(`{invalid}`),
		}
		err := req.Validate()
		assertTrue(t, err != nil)
		if err != nil {
			assertEqual(t, err.Code, niro.ErrCodeInvalidSchema)
		}
	})

	t.Run("InvalidOptions", func(t *testing.T) {
		req := &niro.Request{
			Messages: []niro.Message{niro.UserText("hi")},
			Options: niro.Options{
				Temperature: niro.Temp(3.0), // out of range
			},
		}
		err := req.Validate()
		assertTrue(t, err != nil)
		assertTrue(t, strings.Contains(err.Message, "Temperature"))
	})

	t.Run("InvalidMessage", func(t *testing.T) {
		req := &niro.Request{
			Messages: []niro.Message{
				{Role: niro.RoleUser, Parts: []niro.Part{}}, // empty parts
			},
		}
		err := req.Validate()
		assertNotNil(t, err)
	})
}

func TestMessageValidation(t *testing.T) {
	t.Parallel()

	t.Run("ValidMessage", func(t *testing.T) {
		msg := niro.UserText("hello")
		err := msg.Validate()
		assertNil(t, err)
	})

	t.Run("EmptyMessage", func(t *testing.T) {
		msg := niro.Message{Role: niro.RoleUser, Parts: []niro.Part{}}
		err := msg.Validate()
		assertNotNil(t, err)
	})

	t.Run("InvalidPart", func(t *testing.T) {
		msg := niro.Message{
			Role:  niro.RoleUser,
			Parts: []niro.Part{{Kind: niro.KindImage}}, // image with no data or URL
		}
		err := msg.Validate()
		assertNotNil(t, err)
	})
}

func TestPartValidation(t *testing.T) {
	t.Parallel()

	t.Run("TextPart", func(t *testing.T) {
		p := niro.TextPart("hello")
		assertNil(t, p.Validate())
	})

	t.Run("ImageWithData", func(t *testing.T) {
		p := niro.ImagePart([]byte{0xFF}, "image/jpeg")
		assertNil(t, p.Validate())
	})

	t.Run("ImageURLOnly", func(t *testing.T) {
		p := niro.Part{Kind: niro.KindImage, URL: "https://example.com/img.png"}
		assertNil(t, p.Validate())
	})

	t.Run("AudioNoData", func(t *testing.T) {
		p := niro.Part{Kind: niro.KindAudio}
		assertNotNil(t, p.Validate())
	})

	t.Run("AudioDataNoMime", func(t *testing.T) {
		p := niro.Part{Kind: niro.KindAudio, Data: []byte{0x01}}
		assertNotNil(t, p.Validate())
	})

	t.Run("VideoWithData", func(t *testing.T) {
		p := niro.VideoPart([]byte{0x00}, "video/mp4")
		assertNil(t, p.Validate())
	})

	t.Run("ToolCallNilTool", func(t *testing.T) {
		p := niro.Part{Kind: niro.KindToolCall, Tool: nil}
		assertNotNil(t, p.Validate())
	})

	t.Run("ToolCallValid", func(t *testing.T) {
		p := niro.ToolCallPart(&niro.ToolCall{ID: "c1", Name: "fn"})
		assertNil(t, p.Validate())
	})

	t.Run("ToolResultNil", func(t *testing.T) {
		p := niro.Part{Kind: niro.KindToolResult, Result: nil}
		assertNotNil(t, p.Validate())
	})

	t.Run("ToolResultValid", func(t *testing.T) {
		p := niro.ToolResultPart(&niro.ToolResult{CallID: "c1"})
		assertNil(t, p.Validate())
	})

	t.Run("CustomNil", func(t *testing.T) {
		p := niro.Part{Kind: niro.KindCustom}
		assertNotNil(t, p.Validate())
	})

	t.Run("CustomMissingType", func(t *testing.T) {
		p := niro.Part{
			Kind:   niro.KindCustom,
			Custom: &niro.ExperimentalFrame{Data: "x"},
		}
		assertNotNil(t, p.Validate())
	})

	t.Run("CustomValid", func(t *testing.T) {
		p := niro.CustomPart(&niro.ExperimentalFrame{Type: "reasoning_summary", Data: "x"})
		assertNil(t, p.Validate())
	})

	t.Run("UnknownKind", func(t *testing.T) {
		p := niro.Part{Kind: niro.Kind(99)}
		assertNotNil(t, p.Validate())
	})
}

func TestToolValidation(t *testing.T) {
	t.Parallel()

	t.Run("ValidTool", func(t *testing.T) {
		tool := niro.Tool{
			Name:        "weather",
			Description: "Get weather",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}
		err := tool.Validate()
		assertNil(t, err)
	})

	t.Run("MissingName", func(t *testing.T) {
		tool := niro.Tool{Description: "Get weather"}
		err := tool.Validate()
		assertNotNil(t, err)
		assertTrue(t, strings.Contains(err.Error(), "Name"))
	})

	t.Run("MissingDescription", func(t *testing.T) {
		tool := niro.Tool{Name: "weather"}
		err := tool.Validate()
		assertNotNil(t, err)
		assertTrue(t, strings.Contains(err.Error(), "Description"))
	})

	t.Run("InvalidParameters", func(t *testing.T) {
		tool := niro.Tool{
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
		tc := &niro.ToolCall{ID: "c1", Name: "fn", Args: json.RawMessage(`{}`)}
		assertNil(t, tc.Validate())
	})

	t.Run("MissingID", func(t *testing.T) {
		tc := &niro.ToolCall{Name: "fn"}
		assertNotNil(t, tc.Validate())
	})

	t.Run("MissingName", func(t *testing.T) {
		tc := &niro.ToolCall{ID: "c1"}
		assertNotNil(t, tc.Validate())
	})

	t.Run("InvalidArgs", func(t *testing.T) {
		tc := &niro.ToolCall{ID: "c1", Name: "fn", Args: json.RawMessage(`{invalid}`)}
		assertNotNil(t, tc.Validate())
	})
}

func TestToolResultValidation(t *testing.T) {
	t.Parallel()

	t.Run("Valid", func(t *testing.T) {
		tr := &niro.ToolResult{CallID: "c1", Content: "ok"}
		assertNil(t, tr.Validate())
	})

	t.Run("MissingCallID", func(t *testing.T) {
		tr := &niro.ToolResult{Content: "ok"}
		assertNotNil(t, tr.Validate())
	})
}

func TestToolChoiceValidation(t *testing.T) {
	t.Parallel()

	t.Run("Empty", func(t *testing.T) {
		assertNil(t, niro.ToolChoice("").Validate())
	})

	t.Run("Auto", func(t *testing.T) {
		assertNil(t, niro.ToolChoiceAuto.Validate())
	})

	t.Run("Required", func(t *testing.T) {
		assertNil(t, niro.ToolChoiceRequired.Validate())
	})

	t.Run("None", func(t *testing.T) {
		assertNil(t, niro.ToolChoiceNone.Validate())
	})

	t.Run("FuncPattern", func(t *testing.T) {
		assertNil(t, niro.ToolChoiceFunc("weather").Validate())
	})

	t.Run("Invalid", func(t *testing.T) {
		assertNotNil(t, niro.ToolChoice("invalid_choice").Validate())
	})
}

func TestOptionsValidation(t *testing.T) {
	t.Parallel()

	t.Run("Nil", func(t *testing.T) {
		var o *niro.Options
		assertNil(t, o.Validate())
	})

	t.Run("NegativeMaxTokens", func(t *testing.T) {
		o := &niro.Options{MaxTokens: -1}
		assertNotNil(t, o.Validate())
	})

	t.Run("ValidTemperature", func(t *testing.T) {
		o := &niro.Options{Temperature: niro.Temp(0.5)}
		assertNil(t, o.Validate())
	})

	t.Run("TooHighTemperature", func(t *testing.T) {
		o := &niro.Options{Temperature: niro.Temp(2.5)}
		assertNotNil(t, o.Validate())
	})

	t.Run("NegativeTemperature", func(t *testing.T) {
		o := &niro.Options{Temperature: niro.Temp(-0.1)}
		assertNotNil(t, o.Validate())
	})

	t.Run("ValidTopP", func(t *testing.T) {
		o := &niro.Options{TopP: niro.TopPVal(0.9)}
		assertNil(t, o.Validate())
	})

	t.Run("TooHighTopP", func(t *testing.T) {
		o := &niro.Options{TopP: niro.TopPVal(1.1)}
		assertNotNil(t, o.Validate())
	})

	t.Run("NegativeTopP", func(t *testing.T) {
		o := &niro.Options{TopP: niro.TopPVal(-0.1)}
		assertNotNil(t, o.Validate())
	})

	t.Run("ValidTopK", func(t *testing.T) {
		o := &niro.Options{TopK: niro.TopKVal(40)}
		assertNil(t, o.Validate())
	})

	t.Run("NegativeTopK", func(t *testing.T) {
		o := &niro.Options{TopK: niro.TopKVal(-1)}
		assertNotNil(t, o.Validate())
	})

	t.Run("FrequencyPenaltyOutOfRange", func(t *testing.T) {
		fp := 3.0
		o := &niro.Options{FrequencyPenalty: &fp}
		assertNotNil(t, o.Validate())
	})

	t.Run("PresencePenaltyOutOfRange", func(t *testing.T) {
		pp := -3.0
		o := &niro.Options{PresencePenalty: &pp}
		assertNotNil(t, o.Validate())
	})

	t.Run("ValidPenalties", func(t *testing.T) {
		fp := 1.5
		pp := -1.5
		o := &niro.Options{FrequencyPenalty: &fp, PresencePenalty: &pp}
		assertNil(t, o.Validate())
	})
}

func TestRequestWithToolValidation(t *testing.T) {
	t.Parallel()

	t.Run("ToolChoiceWithTools", func(t *testing.T) {
		req := &niro.Request{
			Messages: []niro.Message{niro.UserText("hi")},
			Tools: []niro.Tool{{
				Name:        "weather",
				Description: "Get weather",
			}},
			ToolChoice: niro.ToolChoiceAuto,
		}
		err := req.Validate()
		assertNil(t, err)
	})

	t.Run("InvalidToolChoiceWithTools", func(t *testing.T) {
		req := &niro.Request{
			Messages: []niro.Message{niro.UserText("hi")},
			Tools: []niro.Tool{{
				Name:        "weather",
				Description: "Get weather",
			}},
			ToolChoice: niro.ToolChoice("bad_choice"),
		}
		err := req.Validate()
		assertNotNil(t, err)
	})

	t.Run("InvalidTool", func(t *testing.T) {
		req := &niro.Request{
			Messages: []niro.Message{niro.UserText("hi")},
			Tools:    []niro.Tool{{Name: "weather"}}, // missing description
		}
		err := req.Validate()
		assertNotNil(t, err)
	})
}
