package openrouter

import (
	"context"
	"encoding/base64"
	"strings"

	orsdk "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"

	"github.com/alexedtionweb/niro-stream"
)

const defaultDecisionModel = "typesafe/jev-1.13"

// DecisionRequestHook allows modifying the raw SDK decisions request before it is sent.
type DecisionRequestHook func(req *components.DecisionsRequest)

// Decide implements [niro.DecisionProvider].
//
// Calls POST /api/alpha/decisions and returns typed answers for each question.
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

	params, err := buildDecisionsRequest(model, req)
	if err != nil {
		return nil, err
	}

	for _, h := range p.decisionHooks {
		h(&params)
	}
	if hook, ok := req.Extra.(DecisionRequestHook); ok {
		hook(&params)
	}

	var opts []operations.Option
	if base := decisionsBaseURL(p.serverURL); base != "" {
		opts = append(opts, operations.WithServerURL(base))
	}

	res, err := p.client.Alpha.Decisions.Create(ctx, params, opts...)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeProviderError, "openrouter decisions", err).WithProvider("openrouter")
	}
	if res == nil {
		return nil, niro.NewError(niro.ErrCodeProviderError, "openrouter: empty decisions response").WithProvider("openrouter")
	}
	return mapDecisionResult(res), nil
}

func buildDecisionsRequest(model string, req *niro.DecisionRequest) (components.DecisionsRequest, error) {
	params := components.DecisionsRequest{
		Model:     model,
		Questions: make(map[string]components.Questions, len(req.Questions)),
	}

	state, err := mapDecisionState(req)
	if err != nil {
		return params, err
	}
	params.State = state

	for id, q := range req.Questions {
		sq, err := mapDecisionQuestion(q)
		if err != nil {
			return params, niro.WrapError(niro.ErrCodeInvalidRequest, "question "+id, err)
		}
		params.Questions[id] = sq
	}

	if len(req.Assets) > 0 {
		params.Assets = make(map[string]components.DecisionsImageAsset, len(req.Assets))
		for key, asset := range req.Assets {
			format := strings.ToLower(strings.TrimSpace(asset.Format))
			if format == "" {
				format = "png"
			}
			if len(asset.Data) == 0 {
				return params, niro.NewError(niro.ErrCodeInvalidRequest, "asset "+key+": empty data")
			}
			t := components.DecisionsImageAssetTypeImage
			params.Assets[key] = components.DecisionsImageAsset{
				DataBase64: base64.StdEncoding.EncodeToString(asset.Data),
				Format:     components.DecisionsImageAssetFormat(format),
				Type:       &t,
			}
		}
	}

	if req.SessionID != "" {
		params.SessionID = orsdk.Pointer(req.SessionID)
	}
	if req.User != "" {
		params.User = orsdk.Pointer(req.User)
	}
	return params, nil
}

func mapDecisionState(req *niro.DecisionRequest) (components.State, error) {
	if req.StateData != nil {
		switch v := req.StateData.(type) {
		case string:
			return components.CreateStateStr(v), nil
		case map[string]any:
			return components.CreateStateMapOfAny(v), nil
		case []any:
			return components.CreateStateArrayOfAny(v), nil
		default:
			return components.State{}, niro.NewError(niro.ErrCodeInvalidRequest, "StateData must be string, map[string]any, or []any")
		}
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return components.State{}, niro.NewError(niro.ErrCodeInvalidRequest, "empty state")
	}
	return components.CreateStateStr(state), nil
}

func mapDecisionQuestion(q niro.DecisionQuestion) (components.Questions, error) {
	instr := strings.TrimSpace(q.Instructions)
	if instr == "" {
		return components.Questions{}, niro.NewError(niro.ErrCodeInvalidRequest, "empty instructions")
	}

	switch q.Kind {
	case niro.DecisionNoul, "":
		nq := components.DecisionsNoulQuestion{
			Instructions: components.CreateDecisionsNoulQuestionInstructionsStr(instr),
			Type:         components.DecisionsNoulQuestionTypeNoul,
		}
		if q.TrueCriteria != "" || q.FalseCriteria != "" {
			nq.Criteria = &components.DecisionsNoulQuestionCriteria{
				True:  components.CreateTrueStr(q.TrueCriteria),
				False: components.CreateFalseStr(q.FalseCriteria),
			}
		}
		return components.CreateQuestionsNoul(nq), nil

	case niro.DecisionChoice:
		if len(q.Criteria) == 0 {
			return components.Questions{}, niro.NewError(niro.ErrCodeInvalidRequest, "choice requires criteria")
		}
		crit := make(map[string]*components.Criteria, len(q.Criteria))
		for label, guidance := range q.Criteria {
			c := components.CreateCriteriaStr(guidance)
			crit[label] = &c
		}
		return components.CreateQuestionsChoice(components.DecisionsChoiceQuestion{
			Criteria:     crit,
			Instructions: components.CreateDecisionsChoiceQuestionInstructionsStr(instr),
			Type:         components.DecisionsChoiceQuestionTypeChoice,
		}), nil

	case niro.DecisionScore:
		if len(q.Levels) == 0 {
			return components.Questions{}, niro.NewError(niro.ErrCodeInvalidRequest, "score requires levels")
		}
		levels := make([]components.Criterion, 0, len(q.Levels))
		for _, level := range q.Levels {
			levels = append(levels, components.CreateCriterionStr(level))
		}
		return components.CreateQuestionsScore(components.DecisionsScoreQuestion{
			Criteria:     levels,
			Instructions: components.CreateDecisionsScoreQuestionInstructionsStr(instr),
			Type:         components.DecisionsScoreQuestionTypeScore,
		}), nil

	case niro.DecisionBox:
		return components.CreateQuestionsBox(components.DecisionsBoxQuestion{
			Instructions: components.CreateDecisionsBoxQuestionInstructionsStr(instr),
			Type:         components.DecisionsBoxQuestionTypeBox,
		}), nil

	default:
		return components.Questions{}, niro.NewError(niro.ErrCodeInvalidRequest, "unknown question kind: "+string(q.Kind))
	}
}

func mapDecisionResult(res *components.DecisionsResponse) *niro.DecisionResult {
	out := &niro.DecisionResult{
		Model:    res.Model,
		Answers:  make(map[string]niro.DecisionAnswer, len(res.Answers)),
		Usage:    decisionUsage(res.Usage),
		Provider: "openrouter",
	}
	if res.ID != nil {
		out.ID = *res.ID
	}
	if res.Provider != nil && *res.Provider != "" {
		out.Provider = *res.Provider
	}
	for id, ans := range res.Answers {
		out.Answers[id] = mapDecisionAnswer(ans)
	}
	return out
}

func mapDecisionAnswer(ans components.DecisionsResponseAnswers) niro.DecisionAnswer {
	switch ans.Type {
	case components.DecisionsResponseAnswersTypeNoul:
		a := niro.DecisionAnswer{Kind: niro.DecisionNoul}
		if ans.DecisionsNoulAnswer != nil {
			a.Noul = ans.DecisionsNoulAnswer.Noul
		}
		return a

	case components.DecisionsResponseAnswersTypeChoice:
		a := niro.DecisionAnswer{Kind: niro.DecisionChoice}
		if ans.DecisionsChoiceAnswer != nil {
			a.Choice = ans.DecisionsChoiceAnswer.Choice
			a.Confidence = ans.DecisionsChoiceAnswer.Confidence
			a.Probabilities = ans.DecisionsChoiceAnswer.Probabilities
		}
		return a

	case components.DecisionsResponseAnswersTypeScore:
		a := niro.DecisionAnswer{Kind: niro.DecisionScore}
		if ans.DecisionsScoreAnswer != nil {
			a.Score = ans.DecisionsScoreAnswer.Score
			a.Confidence = ans.DecisionsScoreAnswer.Confidence
			a.Probabilities = ans.DecisionsScoreAnswer.Probabilities
			if len(ans.DecisionsScoreAnswer.Legend) > 0 {
				a.Legend = make(map[string]string, len(ans.DecisionsScoreAnswer.Legend))
				for k, leg := range ans.DecisionsScoreAnswer.Legend {
					if leg.Str != nil {
						a.Legend[k] = *leg.Str
					}
				}
			}
		}
		return a

	case components.DecisionsResponseAnswersTypeBox:
		a := niro.DecisionAnswer{Kind: niro.DecisionBox}
		if ans.DecisionsBoxAnswer != nil {
			a.Confidence = ans.DecisionsBoxAnswer.Confidence
			a.Box = &niro.BoundingBox{
				Top:    ans.DecisionsBoxAnswer.Box.Top,
				Bottom: ans.DecisionsBoxAnswer.Box.Bottom,
				Left:   ans.DecisionsBoxAnswer.Box.Left,
				Right:  ans.DecisionsBoxAnswer.Box.Right,
			}
		}
		return a

	case components.DecisionsResponseAnswersTypeRefusal:
		return niro.DecisionAnswer{Kind: niro.DecisionRefusal, Refused: true}

	default:
		return niro.DecisionAnswer{Kind: niro.DecisionKind(ans.Type)}
	}
}

func decisionUsage(u components.DecisionsResponseUsage) niro.Usage {
	usage := niro.Usage{
		InputTokens:  int(u.InputTokens),
		OutputTokens: int(u.OutputTokens),
		TotalTokens:  int(u.InputTokens + u.OutputTokens),
	}
	if u.Cost != nil {
		if usage.Detail == nil {
			usage.Detail = map[string]int{}
		}
		usage.Detail[niro.UsageCostMicros] = int(*u.Cost * 1e6)
	}
	return usage
}

// decisionsBaseURL derives the Alpha Decisions host from a chat API base.
// Chat uses …/api/v1; Decisions uses host-rooted /api/alpha/decisions.
func decisionsBaseURL(serverURL string) string {
	base := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if base == "" {
		return ""
	}
	base = strings.TrimSuffix(base, "/api/v1")
	base = strings.TrimSuffix(base, "/v1")
	return strings.TrimRight(base, "/")
}
