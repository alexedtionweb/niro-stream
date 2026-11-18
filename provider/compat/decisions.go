package compat

import (
	"context"

	"github.com/alexedtionweb/niro-stream"
)

// Decide implements [niro.DecisionProvider] via [niro.DecideViaChat].
//
// Suitable for Ollama and other OpenAI-compatible chat endpoints that lack a
// native Decisions API.
func (p *Provider) Decide(ctx context.Context, req *niro.DecisionRequest) (*niro.DecisionResult, error) {
	if req == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "nil request")
	}
	chatReq := *req
	if chatReq.Model == "" {
		chatReq.Model = p.model
	}
	res, err := niro.DecideViaChat(ctx, p, &chatReq)
	if err != nil {
		return nil, err
	}
	if res.Provider == "" {
		res.Provider = "compat"
	}
	return res, nil
}
