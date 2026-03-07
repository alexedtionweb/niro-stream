package niro

import (
	"fmt"
	"regexp"
)

// toolNameRe is the cross-provider safe tool name pattern.
//
// The most restrictive major provider (Amazon Bedrock) requires:
//   - First character must be a letter (a-z or A-Z).
//   - Remaining characters must be letters, digits, or underscores.
//
// OpenAI and Anthropic allow hyphens too, but using this stricter pattern
// guarantees compatibility with all providers without needing per-provider
// validation logic. Use underscores instead of hyphens in tool names.
var toolNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// Validate checks if the Request is valid and returns a detailed Error if not.
// This should be called before invoking Provider.Generate.
func (r *Request) Validate() *Error {
	if r == nil {
		return NewError(ErrCodeInvalidRequest, "request is nil")
	}

	// Check for empty messages (both SystemPrompt and Messages missing)
	if r.SystemPrompt == "" && len(r.Messages) == 0 {
		return NewError(ErrCodeInvalidRequest, "request has no messages (both SystemPrompt and Messages are empty)")
	}

	// Validate each message
	for i, msg := range r.Messages {
		if err := msg.Validate(); err != nil {
			return NewErrorf(ErrCodeInvalidRequest, "message[%d]: %v", i, err)
		}
	}

	// Validate ResponseFormat if set
	if r.ResponseFormat != "" {
		if err := validateResponseFormat(r.ResponseFormat); err != nil {
			return NewErrorf(ErrCodeInvalidRequest, "invalid ResponseFormat: %v", err)
		}

		// If json_schema, require ResponseSchema
		if r.ResponseFormat == "json_schema" && len(r.ResponseSchema) == 0 {
			return NewError(ErrCodeInvalidRequest, "ResponseFormat is json_schema but ResponseSchema is empty")
		}

		// If ResponseSchema is provided, validate it's valid JSON
		if len(r.ResponseSchema) > 0 {
			if !JSONValid(r.ResponseSchema) {
				return NewError(ErrCodeInvalidSchema, "ResponseSchema is not valid JSON")
			}
		}
	}

	// Validate tools if present
	for i, tool := range r.Tools {
		if err := tool.Validate(); err != nil {
			return NewErrorf(ErrCodeInvalidRequest, "tool[%d]: %v", i, err)
		}
	}

	// Validate ToolChoice if tools are present
	if len(r.Tools) > 0 {
		if err := r.ToolChoice.Validate(); err != nil {
			return NewErrorf(ErrCodeInvalidRequest, "invalid ToolChoice: %v", err)
		}
	}

	// Validate Options
	if err := r.Options.Validate(); err != nil {
		return NewErrorf(ErrCodeInvalidRequest, "invalid Options: %v", err)
	}

	return nil
}

// validateResponseFormat checks if the format string is a known value.
func validateResponseFormat(format string) error {
	switch format {
	case "", "json", "json_schema", "text":
		return nil
	default:
		return fmt.Errorf("unknown format %q (must be one of: '', 'json', 'json_schema', 'text')", format)
	}
}

// Validate checks if a Message is valid.
func (m *Message) Validate() error {
	if len(m.Parts) == 0 {
		return fmt.Errorf("message has no parts")
	}

	for i, part := range m.Parts {
		if err := part.Validate(); err != nil {
			return fmt.Errorf("part[%d]: %v", i, err)
		}
	}

	return nil
}

// Validate checks if a Part is valid.
func (p *Part) Validate() error {
	switch p.Kind {
	case KindText:
		// Text is allowed to be empty (could be tool result reference only)
		return nil

	case KindAudio, KindImage, KindVideo:
		if len(p.Data) == 0 && p.URL == "" {
			return fmt.Errorf("binary kind %q requires either Data or URL", p.Kind)
		}
		if p.Mime == "" && len(p.Data) > 0 {
			return fmt.Errorf("binary kind %q with Data requires Mime type", p.Kind)
		}
		return nil

	case KindToolCall:
		if p.Tool == nil {
			return fmt.Errorf("KindToolCall requires Tool")
		}
		return p.Tool.Validate()

	case KindToolResult:
		if p.Result == nil {
			return fmt.Errorf("KindToolResult requires Result")
		}
		return p.Result.Validate()

	case KindCustom:
		if p.Custom == nil {
			return fmt.Errorf("KindCustom requires Custom payload")
		}
		if p.Custom.Type == "" {
			return fmt.Errorf("KindCustom requires Custom.Type")
		}
		return nil

	default:
		return fmt.Errorf("unknown Kind %d", p.Kind)
	}
}

// Validate checks if a Tool definition is valid.
func (t *Tool) Validate() error {
	if t.Name == "" {
		return fmt.Errorf("Tool.Name is required")
	}
	if !toolNameRe.MatchString(t.Name) {
		return fmt.Errorf("Tool.Name %q is invalid: must start with a letter and contain only letters, digits, and underscores (required for cross-provider compatibility)", t.Name)
	}
	if t.Description == "" {
		return fmt.Errorf("Tool.Description is required")
	}
	if len(t.Parameters) > 0 && !JSONValid(t.Parameters) {
		return fmt.Errorf("Tool.Parameters must be valid JSON schema")
	}
	return nil
}

// Validate checks if a ToolCall is valid.
func (tc *ToolCall) Validate() error {
	if tc.ID == "" {
		return fmt.Errorf("ToolCall.ID is required")
	}
	if tc.Name == "" {
		return fmt.Errorf("ToolCall.Name is required")
	}
	if len(tc.Args) > 0 && !JSONValid(tc.Args) {
		return fmt.Errorf("ToolCall.Args must be valid JSON")
	}
	return nil
}

// Validate checks if a ToolResult is valid.
func (tr *ToolResult) Validate() error {
	if tr.CallID == "" {
		return fmt.Errorf("ToolResult.CallID is required")
	}
	// Content can be empty, IsError is a flag
	return nil
}

// Validate checks if ToolChoice is valid when tools are present.
func (tc ToolChoice) Validate() error {
	switch tc {
	case "", ToolChoiceAuto, ToolChoiceRequired, ToolChoiceNone:
		return nil
	default:
		// Accept func:name pattern created by ToolChoiceFunc()
		if len(tc) > 5 && tc[:5] == "func:" {
			return nil
		}
		return fmt.Errorf("unknown ToolChoice %q", tc)
	}
}

// Validate checks if generation Options are valid.
func (o *Options) Validate() error {
	if o == nil {
		return nil
	}

	// Validate MaxTokens
	if o.MaxTokens < 0 {
		return fmt.Errorf("maxTokens must be >= 0")
	}

	// Validate Temperature
	if o.Temperature != nil {
		if *o.Temperature < 0 || *o.Temperature > 2.0 {
			return fmt.Errorf("temperature must be in range [0, 2.0], got %v", *o.Temperature)
		}
	}

	// Validate TopP
	if o.TopP != nil {
		if *o.TopP < 0 || *o.TopP > 1.0 {
			return fmt.Errorf("topP must be in range [0, 1.0], got %v", *o.TopP)
		}
	}

	// Validate TopK
	if o.TopK != nil {
		if *o.TopK < 0 {
			return fmt.Errorf("topK must be >= 0, got %v", *o.TopK)
		}
	}

	// Validate FrequencyPenalty
	if o.FrequencyPenalty != nil {
		if *o.FrequencyPenalty < -2.0 || *o.FrequencyPenalty > 2.0 {
			return fmt.Errorf("frequencyPenalty must be in range [-2.0, 2.0], got %v", *o.FrequencyPenalty)
		}
	}

	// Validate PresencePenalty
	if o.PresencePenalty != nil {
		if *o.PresencePenalty < -2.0 || *o.PresencePenalty > 2.0 {
			return fmt.Errorf("presencePenalty must be in range [-2.0, 2.0], got %v", *o.PresencePenalty)
		}
	}

	// Validate cache options
	if err := validateCacheOptions(o.Cache); err != nil {
		return fmt.Errorf("cache: %w", err)
	}

	return nil
}
