package niro

import (
	"context"
	"encoding/json"
	"strings"
)

// DecideViaChat answers [DecisionRequest] questions using a chat [Provider]
// and JSON output. Used by OpenAI-compatible backends (Ollama, vLLM, etc.)
// and as a fallback when a native Decisions API is unavailable.
//
// Box questions are not supported via chat; use a native Decisions provider.
func DecideViaChat(ctx context.Context, p Provider, req *DecisionRequest) (*DecisionResult, error) {
	if p == nil {
		return nil, NewError(ErrCodeInvalidRequest, "nil provider")
	}
	if req == nil {
		return nil, NewError(ErrCodeInvalidRequest, "nil request")
	}
	if len(req.Questions) == 0 {
		return nil, NewError(ErrCodeInvalidRequest, "no questions")
	}
	for id, q := range req.Questions {
		if q.Kind == DecisionBox {
			return nil, NewErrorf(ErrCodeInvalidRequest, "question %s: box is not supported via chat decisions", id)
		}
	}

	stateText, err := formatDecisionState(req)
	if err != nil {
		return nil, err
	}

	schema, err := decisionChatSchema(req.Questions)
	if err != nil {
		return nil, err
	}
	prompt, err := decisionChatPrompt(req.Questions)
	if err != nil {
		return nil, err
	}

	parts := []Part{TextPart(stateText)}
	for key, asset := range req.Assets {
		if len(asset.Data) == 0 {
			continue
		}
		mime := "image/png"
		switch strings.ToLower(asset.Format) {
		case "jpg", "jpeg":
			mime = "image/jpeg"
		}
		parts = append(parts, TextPart("asset:"+key), ImagePart(asset.Data, mime))
	}

	chatReq := &Request{
		Model: req.Model,
		SystemPrompt: "You are a decisions engine. Answer every question about the given state. " +
			"Respond with a single JSON object only — no markdown.",
		Messages: []Message{{
			Role:  RoleUser,
			Parts: append([]Part{TextPart(prompt)}, parts...),
		}},
		ResponseFormat: "json_schema",
		ResponseSchema: schema,
		Options:        Options{Temperature: Temp(0)},
	}

	stream, err := p.Generate(ctx, chatReq)
	if err != nil {
		// Some local servers reject json_schema; retry with plain JSON mode.
		chatReq.ResponseFormat = "json"
		chatReq.ResponseSchema = nil
		stream, err = p.Generate(ctx, chatReq)
		if err != nil {
			return nil, err
		}
	}

	text, err := CollectText(ctx, stream)
	if err != nil {
		return nil, err
	}
	raw := extractJSONObject([]byte(text))
	if len(raw) == 0 {
		return nil, NewError(ErrCodeProviderError, "empty decisions chat response")
	}

	var parsed struct {
		Answers map[string]decisionChatAnswer `json:"answers"`
	}
	if err := JSONUnmarshal(raw, &parsed); err != nil {
		return nil, WrapError(ErrCodeProviderError, "decode decisions chat JSON", err)
	}
	if parsed.Answers == nil {
		return nil, NewError(ErrCodeProviderError, "decisions chat response missing answers")
	}

	out := &DecisionResult{
		Answers: make(map[string]DecisionAnswer, len(parsed.Answers)),
		Usage:   stream.Usage(),
	}
	if meta := stream.Response(); meta != nil {
		out.ID = meta.ID
		out.Model = meta.Model
	}
	if out.Model == "" {
		out.Model = req.Model
	}
	for id, a := range parsed.Answers {
		out.Answers[id] = a.toDecisionAnswer()
	}
	return out, nil
}

type decisionChatAnswer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Refused       bool               `json:"refused,omitempty"`
}

func (a decisionChatAnswer) toDecisionAnswer() DecisionAnswer {
	kind := DecisionKind(a.Type)
	if a.Refused || kind == DecisionRefusal {
		return DecisionAnswer{Kind: DecisionRefusal, Refused: true}
	}
	switch kind {
	case DecisionNoul, "":
		return DecisionAnswer{Kind: DecisionNoul, Noul: a.Noul, Confidence: a.Confidence, Probabilities: a.Probabilities}
	case DecisionChoice:
		return DecisionAnswer{Kind: DecisionChoice, Choice: a.Choice, Confidence: a.Confidence, Probabilities: a.Probabilities}
	case DecisionScore:
		return DecisionAnswer{Kind: DecisionScore, Score: a.Score, Confidence: a.Confidence, Probabilities: a.Probabilities}
	default:
		return DecisionAnswer{Kind: kind, Confidence: a.Confidence, Probabilities: a.Probabilities}
	}
}

func formatDecisionState(req *DecisionRequest) (string, error) {
	if req.StateData != nil {
		switch v := req.StateData.(type) {
		case string:
			if strings.TrimSpace(v) == "" {
				return "", NewError(ErrCodeInvalidRequest, "empty state")
			}
			return v, nil
		default:
			b, err := JSONMarshal(v)
			if err != nil {
				return "", WrapError(ErrCodeInvalidRequest, "marshal StateData", err)
			}
			return string(b), nil
		}
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return "", NewError(ErrCodeInvalidRequest, "empty state")
	}
	return state, nil
}

func decisionChatPrompt(questions map[string]DecisionQuestion) (string, error) {
	type qSpec struct {
		ID           string            `json:"id"`
		Type         DecisionKind      `json:"type"`
		Instructions string            `json:"instructions"`
		True         string            `json:"true_when,omitempty"`
		False        string            `json:"false_when,omitempty"`
		Criteria     map[string]string `json:"criteria,omitempty"`
		Levels       []string          `json:"levels,omitempty"`
	}
	specs := make([]qSpec, 0, len(questions))
	for id, q := range questions {
		kind := q.Kind
		if kind == "" {
			kind = DecisionNoul
		}
		instr := strings.TrimSpace(q.Instructions)
		if instr == "" {
			return "", NewErrorf(ErrCodeInvalidRequest, "question %s: empty instructions", id)
		}
		spec := qSpec{ID: id, Type: kind, Instructions: instr}
		switch kind {
		case DecisionNoul:
			spec.True, spec.False = q.TrueCriteria, q.FalseCriteria
		case DecisionChoice:
			if len(q.Criteria) == 0 {
				return "", NewErrorf(ErrCodeInvalidRequest, "question %s: choice requires criteria", id)
			}
			spec.Criteria = q.Criteria
		case DecisionScore:
			if len(q.Levels) == 0 {
				return "", NewErrorf(ErrCodeInvalidRequest, "question %s: score requires levels", id)
			}
			spec.Levels = q.Levels
		default:
			return "", NewErrorf(ErrCodeInvalidRequest, "question %s: unsupported kind %q", id, kind)
		}
		specs = append(specs, spec)
	}
	b, err := JSONMarshal(specs)
	if err != nil {
		return "", err
	}
	return "Questions (answer every id):\n" + string(b) +
		"\n\nReturn JSON: {\"answers\":{\"<id>\":{\"type\":\"noul|choice|score\",\"noul\":0-1,\"choice\":\"...\",\"score\":number}}}", nil
}

func decisionChatSchema(questions map[string]DecisionQuestion) (json.RawMessage, error) {
	props := map[string]any{}
	required := make([]string, 0, len(questions))
	for id, q := range questions {
		kind := q.Kind
		if kind == "" {
			kind = DecisionNoul
		}
		var answerSchema map[string]any
		switch kind {
		case DecisionNoul:
			answerSchema = map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"type", "noul"},
				"properties": map[string]any{
					"type": map[string]any{"type": "string", "enum": []string{"noul"}},
					"noul": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				},
			}
		case DecisionChoice:
			enums := make([]string, 0, len(q.Criteria))
			for label := range q.Criteria {
				enums = append(enums, label)
			}
			answerSchema = map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"type", "choice"},
				"properties": map[string]any{
					"type":   map[string]any{"type": "string", "enum": []string{"choice"}},
					"choice": map[string]any{"type": "string", "enum": enums},
				},
			}
		case DecisionScore:
			answerSchema = map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"type", "score"},
				"properties": map[string]any{
					"type":  map[string]any{"type": "string", "enum": []string{"score"}},
					"score": map[string]any{"type": "number", "minimum": 0, "maximum": float64(len(q.Levels) - 1)},
				},
			}
		default:
			return nil, NewErrorf(ErrCodeInvalidRequest, "question %s: unsupported kind %q", id, kind)
		}
		props[id] = answerSchema
		required = append(required, id)
	}

	schema := map[string]any{
		"name":   "decision_answers",
		"strict": true,
		"schema": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"answers"},
			"properties": map[string]any{
				"answers": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             required,
					"properties":           props,
				},
			},
		},
	}
	return JSONMarshal(schema)
}

func extractJSONObject(b []byte) []byte {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```JSON")
		s = strings.TrimPrefix(s, "```")
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return nil
	}
	return []byte(s[start : end+1])
}
