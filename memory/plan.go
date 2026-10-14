package memory

import (
	"context"
	"sync"

	"github.com/alexedtionweb/niro-stream"
)

// Target is one model in a plan.
// Scope and View, when set, replace the plan's scope and view for this model
// only. Messages, when non-nil, replace the plan messages.
// Parts are images or video attached to the user turn. See recalls records
// and resolves each Ref through the target Album, or the plan Album.
// Request, when set, builds the provider request from the rendered prompt
// and those parts. The other request fields are not used.
type Target struct {
	Name       string
	Provider   niro.Provider
	Model      string
	Scope      string
	View       *View
	Messages   []niro.Message
	Tools      []niro.Tool
	ToolChoice niro.ToolChoice
	Options    niro.Options
	Parts      []niro.Part
	See        *Query
	Album      Album
	Request    func(prompt string, parts []niro.Part) *niro.Request
}

// Result is one target's output. Prompt is the system prompt that was sent.
// Err is set when that model failed. Other targets still finish unless Race is set.
type Result struct {
	Name   string
	Model  string
	Prompt string
	Text   string
	Usage  niro.Usage
	Err    error
}

// Plan renders a view and runs every target.
// Race returns the first success and cancels the rest. Targets must stop
// when the context is canceled. When every target fails, Race returns each
// failure.
type Plan struct {
	Store    Store
	Scope    string
	View     View
	Messages []niro.Message
	Targets  []Target
	Album    Album
	Race     bool
}

// Run executes the plan.
func (p *Plan) Run(ctx context.Context) ([]Result, error) {
	if p == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "plan is nil")
	}
	if len(p.Targets) == 0 {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "no targets")
	}
	if p.Race {
		return p.race(ctx)
	}
	return p.all(ctx)
}

func (p *Plan) all(ctx context.Context) ([]Result, error) {
	out := make([]Result, len(p.Targets))
	var wg sync.WaitGroup
	wg.Add(len(p.Targets))
	for i, t := range p.Targets {
		go func() {
			defer wg.Done()
			out[i] = p.one(ctx, t)
		}()
	}
	wg.Wait()
	return out, nil
}

func (p *Plan) race(ctx context.Context) ([]Result, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan Result, len(p.Targets))
	for _, t := range p.Targets {
		go func() {
			ch <- p.one(ctx, t)
		}()
	}
	fails := make([]Result, 0, len(p.Targets))
	for range p.Targets {
		r := <-ch
		if r.Err == nil {
			cancel()
			return []Result{r}, nil
		}
		fails = append(fails, r)
	}
	return fails, nil
}

func (p *Plan) one(ctx context.Context, t Target) Result {
	res := Result{Name: t.Name, Model: t.Model}
	if err := ctx.Err(); err != nil {
		res.Err = err
		return res
	}
	if t.Provider == nil {
		res.Err = niro.NewError(niro.ErrCodeInvalidRequest, "target provider is nil")
		return res
	}
	view := p.View
	if t.View != nil {
		view = *t.View
	}
	scope := p.Scope
	if t.Scope != "" {
		scope = t.Scope
	}
	prompt, err := view.Build(ctx, p.Store, scope)
	if err != nil {
		res.Err = err
		return res
	}
	parts, err := p.sights(ctx, t, scope)
	if err != nil {
		res.Err = err
		return res
	}
	req := p.request(t, prompt, parts)
	if t.Request != nil {
		req = t.Request(prompt, parts)
	}
	if req == nil {
		res.Err = niro.NewError(niro.ErrCodeInvalidRequest, "target request is nil")
		return res
	}
	res.Prompt = req.SystemPrompt
	res.Model = req.Model
	stream, err := t.Provider.Generate(ctx, req)
	if err != nil {
		res.Err = err
		return res
	}
	text, err := niro.CollectText(ctx, stream)
	res.Text = text
	res.Usage = stream.Usage()
	res.Err = err
	return res
}

func (p *Plan) sights(ctx context.Context, t Target, scope string) ([]niro.Part, error) {
	var parts []niro.Part
	if t.See != nil {
		if p.Store == nil {
			return nil, errNilStore
		}
		recs, err := p.Store.Recall(ctx, t.See.scoped(scope))
		if err != nil {
			return nil, err
		}
		album := t.Album
		if album == nil {
			album = p.Album
		}
		for _, rec := range recs {
			if rec.Ref == "" {
				continue
			}
			if album == nil {
				return nil, niro.NewError(niro.ErrCodeInvalidRequest, "memory album is nil")
			}
			part, ok, err := album.Open(ctx, rec.Ref)
			if err != nil {
				return nil, err
			}
			if !ok || (len(part.Data) == 0 && part.URL == "") {
				return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest, "missing media %s", rec.Ref)
			}
			parts = append(parts, part)
		}
	}
	parts = append(parts, t.Parts...)
	return parts, nil
}

func (p *Plan) request(t Target, prompt string, parts []niro.Part) *niro.Request {
	msgs := p.Messages
	if t.Messages != nil {
		msgs = t.Messages
	}
	copied := make([]niro.Message, len(msgs))
	copy(copied, msgs)
	return &niro.Request{
		Model:        t.Model,
		SystemPrompt: prompt,
		Messages:     attach(copied, parts),
		Tools:        t.Tools,
		ToolChoice:   t.ToolChoice,
		Options:      t.Options,
	}
}

func attach(msgs []niro.Message, parts []niro.Part) []niro.Message {
	if len(parts) == 0 {
		return msgs
	}
	if len(msgs) == 0 {
		return []niro.Message{niro.Multi(niro.RoleUser, parts...)}
	}
	last := len(msgs) - 1
	if msgs[last].Role != niro.RoleUser {
		return append(msgs, niro.Multi(niro.RoleUser, parts...))
	}
	merged := make([]niro.Part, 0, len(msgs[last].Parts)+len(parts))
	merged = append(merged, msgs[last].Parts...)
	merged = append(merged, parts...)
	msgs[last].Parts = merged
	return msgs
}

// Save writes each successful result back into the plan store.
// The record source is the target name. Empty text is skipped.
func (p *Plan) Save(ctx context.Context, results []Result) ([]Record, error) {
	if p == nil || p.Store == nil {
		return nil, errNilStore
	}
	out := make([]Record, 0, len(results))
	for _, r := range results {
		if r.Err != nil || r.Text == "" {
			continue
		}
		rec, err := p.Store.Put(ctx, Record{Scope: p.Scope, Source: r.Name, Body: r.Text})
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}
