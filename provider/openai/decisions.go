package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/transport"
)

const (
	defaultDecisionModel = "gpt-6-luna"
	defaultOpenAIBaseURL = "https://api.openai.com/v1"
)

// ChatDecisions forces [Provider.Decide] to use [niro.DecideViaChat]
// instead of POST /v1/decisions. Useful for non-luna models.
type ChatDecisions struct{}

// DecisionRequestHook mutates the raw native Decisions JSON body before send.
type DecisionRequestHook func(body map[string]any)

// Decide implements [niro.DecisionProvider].
//
// Uses OpenAI's native Decisions API (POST /v1/decisions) when the model is
// empty or looks like a luna decisions model. Otherwise (or when Extra is
// [ChatDecisions]) falls back to [niro.DecideViaChat].
func (p *Provider) Decide(ctx context.Context, req *niro.DecisionRequest) (*niro.DecisionResult, error) {
	if req == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "nil request")
	}
	if len(req.Questions) == 0 {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "no questions")
	}

	model := req.Model
	if model == "" {
		model = p.decisionModel
	}
	if model == "" {
		model = defaultDecisionModel
	}

	if useChatDecisions(model, req) || p.apiKey == "" {
		chatReq := *req
		chatReq.Model = model
		if chatReq.Model == defaultDecisionModel {
			// Chat fallback needs a chat-completions model.
			chatReq.Model = p.model
		}
		res, err := niro.DecideViaChat(ctx, p, &chatReq)
		if err != nil {
			return nil, err
		}
		if res.Provider == "" {
			res.Provider = "openai"
		}
		return res, nil
	}

	return p.decideNative(ctx, model, req)
}

func useChatDecisions(model string, req *niro.DecisionRequest) bool {
	if _, ok := req.Extra.(ChatDecisions); ok {
		return true
	}
	m := strings.ToLower(model)
	return m != "" && !strings.Contains(m, "luna")
}

func (p *Provider) decideNative(ctx context.Context, model string, req *niro.DecisionRequest) (*niro.DecisionResult, error) {
	for id, q := range req.Questions {
		if q.Kind == niro.DecisionBox {
			return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest, "question %s: openai decisions does not support box", id)
		}
	}

	body, err := buildOpenAIDecisionBody(model, req)
	if err != nil {
		return nil, err
	}
	for _, h := range p.decisionHooks {
		h(body)
	}
	if hook, ok := req.Extra.(DecisionRequestHook); ok {
		hook(body)
	}

	payload, err := niro.JSONMarshal(body)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "marshal decisions", err)
	}

	base := strings.TrimRight(p.baseURL, "/")
	if base == "" {
		base = defaultOpenAIBaseURL
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/decisions", bytes.NewReader(payload))
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "decisions request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	client := p.httpClient
	if client == nil {
		client = transport.DefaultHTTPClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "openai decisions", err).WithProvider("openai")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, niro.NewErrorf(niro.ConvertHTTPStatusToCode(resp.StatusCode), "openai decisions status %d: %s", resp.StatusCode, string(raw)).WithProvider("openai")
	}

	res, err := parseOpenAIDecisionResponse(raw)
	if err != nil {
		return nil, err
	}
	if res.Provider == "" {
		res.Provider = "openai"
	}
	return res, nil
}

func buildOpenAIDecisionBody(model string, req *niro.DecisionRequest) (map[string]any, error) {
	input, err := openAIDecisionInput(req)
	if err != nil {
		return nil, err
	}
	questions := make([]map[string]any, 0, len(req.Questions))
	for id, q := range req.Questions {
		oq, err := openAIDecisionQuestion(id, q)
		if err != nil {
			return nil, err
		}
		questions = append(questions, oq)
	}
	return map[string]any{
		"model":     model,
		"input":     input,
		"questions": questions,
	}, nil
}

func openAIDecisionInput(req *niro.DecisionRequest) (any, error) {
	if len(req.Assets) == 0 && req.StateData == nil {
		state := strings.TrimSpace(req.State)
		if state == "" {
			return nil, niro.NewError(niro.ErrCodeInvalidRequest, "empty state")
		}
		return state, nil
	}

	parts := []map[string]any{}
	if req.StateData != nil {
		switch v := req.StateData.(type) {
		case string:
			if strings.TrimSpace(v) == "" {
				return nil, niro.NewError(niro.ErrCodeInvalidRequest, "empty state")
			}
			parts = append(parts, map[string]any{"type": "input_text", "text": v})
		default:
			parts = append(parts, map[string]any{"type": "input_json", "data": v})
		}
	} else if state := strings.TrimSpace(req.State); state != "" {
		parts = append(parts, map[string]any{"type": "input_text", "text": state})
	} else {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "empty state")
	}

	for key, asset := range req.Assets {
		if len(asset.Data) == 0 {
			return nil, niro.NewError(niro.ErrCodeInvalidRequest, "asset "+key+": empty data")
		}
		mime := "image/png"
		switch strings.ToLower(asset.Format) {
		case "jpg", "jpeg":
			mime = "image/jpeg"
		}
		url := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(asset.Data)
		parts = append(parts, map[string]any{"type": "input_image", "image_url": url})
	}

	return []map[string]any{{
		"role":    "user",
		"content": parts,
	}}, nil
}

func openAIDecisionQuestion(id string, q niro.DecisionQuestion) (map[string]any, error) {
	instr := strings.TrimSpace(q.Instructions)
	if instr == "" {
		return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest, "question %s: empty instructions", id)
	}
	kind := q.Kind
	if kind == "" || kind == niro.DecisionNoul {
		if q.TrueCriteria != "" || q.FalseCriteria != "" {
			instr = instr + "\nTrue when: " + q.TrueCriteria + "\nFalse when: " + q.FalseCriteria
		}
		return map[string]any{
			"type":         "predicate",
			"name":         id,
			"instructions": instr,
		}, nil
	}
	switch kind {
	case niro.DecisionChoice:
		if len(q.Criteria) < 2 {
			return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest, "question %s: choice requires at least 2 criteria", id)
		}
		choices := make([]map[string]any, 0, len(q.Criteria))
		for value, desc := range q.Criteria {
			choices = append(choices, map[string]any{"value": value, "description": desc})
		}
		return map[string]any{
			"type":         "choice",
			"name":         id,
			"instructions": instr,
			"choices":      choices,
		}, nil

	case niro.DecisionScore:
		if len(q.Levels) < 2 {
			return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest, "question %s: score requires at least 2 levels", id)
		}
		levels := make([]map[string]any, 0, len(q.Levels))
		for _, label := range q.Levels {
			levels = append(levels, map[string]any{"label": label})
		}
		return map[string]any{
			"type":         "score",
			"name":         id,
			"instructions": instr,
			"levels":       levels,
		}, nil

	default:
		return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest, "question %s: unsupported kind %q", id, kind)
	}
}

func parseOpenAIDecisionResponse(raw []byte) (*niro.DecisionResult, error) {
	var resp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Answers []struct {
			Type          string             `json:"type"`
			Name          string             `json:"name"`
			Probability   float64            `json:"probability"`
			Choice        any                `json:"choice"`
			Score         float64            `json:"score"`
			Confidence    *float64           `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
		Usage struct {
			InputTokens  int     `json:"input_tokens"`
			OutputTokens int     `json:"output_tokens"`
			TotalTokens  int     `json:"total_tokens"`
			Cost         float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := niro.JSONUnmarshal(raw, &resp); err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "decode openai decisions", err).WithProvider("openai")
	}

	out := &niro.DecisionResult{
		ID:       resp.ID,
		Model:    resp.Model,
		Provider: "openai",
		Answers:  make(map[string]niro.DecisionAnswer, len(resp.Answers)),
		Usage: niro.Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		},
	}
	if out.Usage.TotalTokens == 0 {
		out.Usage.TotalTokens = out.Usage.InputTokens + out.Usage.OutputTokens
	}
	if resp.Usage.Cost > 0 {
		out.Usage.Detail = map[string]int{niro.UsageCostMicros: int(resp.Usage.Cost * 1e6)}
	}

	for _, a := range resp.Answers {
		name := a.Name
		if name == "" {
			name = a.Type
		}
		switch a.Type {
		case "predicate":
			out.Answers[name] = niro.DecisionAnswer{
				Kind:          niro.DecisionNoul,
				Noul:          a.Probability,
				Confidence:    a.Confidence,
				Probabilities: a.Probabilities,
			}
		case "choice":
			choice, _ := a.Choice.(string)
			if choice == "" {
				choice = stringifyAny(a.Choice)
			}
			out.Answers[name] = niro.DecisionAnswer{
				Kind:          niro.DecisionChoice,
				Choice:        choice,
				Confidence:    a.Confidence,
				Probabilities: a.Probabilities,
			}
		case "score":
			out.Answers[name] = niro.DecisionAnswer{
				Kind:          niro.DecisionScore,
				Score:         a.Score,
				Confidence:    a.Confidence,
				Probabilities: a.Probabilities,
			}
		case "refusal":
			out.Answers[name] = niro.DecisionAnswer{Kind: niro.DecisionRefusal, Refused: true}
		default:
			out.Answers[name] = niro.DecisionAnswer{Kind: niro.DecisionKind(a.Type)}
		}
	}
	return out, nil
}

func stringifyAny(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case json.Number:
		return t.String()
	default:
		b, err := niro.JSONMarshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
