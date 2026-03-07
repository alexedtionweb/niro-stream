package dsl

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/hook"
	"github.com/alexedtionweb/niro-stream/orchestrate"
	"github.com/alexedtionweb/niro-stream/output"
	"github.com/alexedtionweb/niro-stream/tools"
)

// AgentTools is the resolved tool responsibility for one agent run: the toolset to attach
// to the request, tool choice, and tool stream options. Composable with niro: toolset.Apply(req)
// then ToolingProvider(provider, toolset, opts).Generate(ctx, req).
type AgentTools struct {
	Toolset    *tools.Toolset
	ToolChoice niro.ToolChoice
	Options    tools.ToolStreamOptions
}

// Runner runs agents from a compiled NiroDefinition using a provider.
// Stream returns a niro.Stream for composition with orchestrate.Fan/Race/Sequence.
// Tools are a composable responsibility; handoff targets are resolved from JSON (workflows + agents) via BindWorkflows and RunHandoff.
type Runner struct {
	nd            *NiroDefinition
	provider      niro.Provider
	workflows     map[string]*CompiledWorkflow
	toolTransform func(agentName string, base *tools.Toolset) *tools.Toolset
	toolOptions   func(agentName string) tools.ToolStreamOptions
	outputSink    *output.Sink // optional: route response/thinking to sinks as stream is consumed
	hook          hook.Hook    // optional: telemetry/observability hook (OnFrame, OnToolCall, OnGenerateEnd, etc.)
}

// RunnerOption customizes the runner (e.g. toolset transform, tool stream options per agent).
type RunnerOption func(*Runner)

// WithToolsetTransform runs after resolving the agent's toolset; use to add approver, hooks, or replace.
func WithToolsetTransform(fn func(agentName string, base *tools.Toolset) *tools.Toolset) RunnerOption {
	return func(r *Runner) {
		r.toolTransform = fn
	}
}

// WithToolOptions overrides tool stream options per agent (max rounds, timeout, approver, etc.).
func WithToolOptions(fn func(agentName string) tools.ToolStreamOptions) RunnerOption {
	return func(r *Runner) {
		r.toolOptions = fn
	}
}

// WithOutputSink routes LLM output to the given sink as the stream is consumed.
// Response text, thinking (KindCustom "thinking"/"reasoning"), tool calls, and custom
// frames are dispatched to the sink's callbacks in a single pass (stream-first, no extra buffering).
// The returned stream still forwards all frames unchanged so handoff and history logic work as before.
func WithOutputSink(sink *output.Sink) RunnerOption {
	return func(r *Runner) {
		r.outputSink = sink
	}
}

// WithHook attaches a telemetry/observability hook to the runner.
// The hook fires OnGenerateStart before each provider call, OnFrame/OnToolCall/OnToolResult
// per frame, and OnGenerateEnd when the stream is exhausted. Use hook.Compose to combine
// multiple hooks (e.g. Langfuse + Datadog).
func WithHook(h hook.Hook) RunnerOption {
	return func(r *Runner) {
		r.hook = h
	}
}

// BindWorkflows registers compiled workflows (from workflow.json) so RunHandoff can execute them by name.
// Call after compiling workflows and binding each to this runner: for _, cw := range workflows { cw.BindRunner(runner) }; runner.BindWorkflows(workflows).
func (r *Runner) BindWorkflows(workflows map[string]*CompiledWorkflow) {
	if r == nil {
		return
	}
	r.workflows = workflows
}

// NewRunner creates a Runner that uses the given provider for all agents.
// Options can customize tools per agent (WithToolsetTransform, WithToolOptions).
func NewRunner(nd *NiroDefinition, provider niro.Provider, opts ...RunnerOption) *Runner {
	if nd == nil {
		nd = &NiroDefinition{Agents: make(map[string]*CompiledAgentConfig), Toolset: tools.NewToolset()}
	}
	r := &Runner{nd: nd, provider: provider}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Stream runs the named agent and returns a stream. RunContext supplies session, event, history
// and custom vars for template expansion and when/unless evaluation.
// Wiring: resolveAgentTools → optional toolset transform → req = toolset.Apply(req) → ToolingProvider.Generate(ctx, req).
func (r *Runner) Stream(ctx context.Context, runCtx *RunContext, agentName string, sessionID string) (*niro.Stream, error) {
	if r == nil || r.nd == nil || r.provider == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "dsl: runner or provider is nil")
	}
	cfg, ok := r.nd.Agents[agentName]
	if !ok {
		return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: agent %q not found", agentName)
	}
	if runCtx == nil {
		runCtx = NewRunContext()
	}

	systemPrompt, err := expandPrompt(cfg.PromptTemplate, runCtx)
	if err != nil {
		return nil, err
	}
	messages, err := r.messagesFromRunContext(runCtx)
	if err != nil {
		return nil, err
	}

	agentTools, err := r.resolveAgentTools(cfg, runCtx, agentName)
	if err != nil {
		return nil, err
	}
	toolset := r.applyToolsetTransform(agentName, agentTools.Toolset)
	opts := r.mergeToolOptions(agentName, agentTools.Options)

	req := &niro.Request{
		Model:        cfg.Model,
		SystemPrompt: systemPrompt,
		Messages:     messages,
		Options:      cfg.Options,
		ToolChoice:   agentTools.ToolChoice,
	}
	req = toolset.Apply(req)

	ctx = WithRunContext(ctx, runCtx)

	start := time.Now()
	if r.hook != nil {
		ctx = r.hook.OnGenerateStart(ctx, hook.GenerateStartInfo{
			Model:    cfg.Model,
			Messages: len(messages),
			Tools:    len(req.Tools),
		})
	}

	stream, err := tools.NewToolingProvider(r.provider, toolset, opts).Generate(ctx, req)
	if err != nil {
		if r.hook != nil {
			r.hook.OnError(ctx, err)
			r.hook.OnGenerateEnd(ctx, hook.GenerateEndInfo{
				Model:    cfg.Model,
				Duration: time.Since(start),
				Error:    err,
			})
		}
		return nil, err
	}

	stream = hook.WrapStream(ctx, stream, r.hook, cfg.Model, start)
	if r.outputSink != nil {
		stream = output.Route(ctx, stream, r.outputSink)
	}
	stream = r.resolveHandoff(ctx, stream, runCtx, sessionID)
	return stream, nil
}

// resolveHandoff wraps a stream to transparently resolve handoff signals.
// When a KindCustom "handoff" frame arrives, it emits a "handoff_start" frame
// (so sinks can reset state), runs the target agent/workflow, and splices
// the target's stream into the output. Supports chained handoffs.
func (r *Runner) resolveHandoff(ctx context.Context, src *niro.Stream, runCtx *RunContext, sessionID string) *niro.Stream {
	out, em := niro.NewStream(niro.DefaultStreamBuffer)
	go func() {
		defer em.Close()
		var classifierText strings.Builder

		for src.Next(ctx) {
			f := src.Frame()

			if f.Kind == niro.KindText {
				classifierText.WriteString(f.Text)
			}

			if f.Kind == niro.KindCustom && f.Custom != nil && f.Custom.Type == niro.CustomHandoff {
				target := strings.TrimSpace(fmt.Sprint(f.Custom.Data))
				if target == "" {
					if err := em.Emit(ctx, f); err != nil {
						return
					}
					continue
				}

				_ = em.Emit(ctx, niro.Frame{
					Kind:   niro.KindCustom,
					Custom: &niro.ExperimentalFrame{Type: niro.CustomHandoffStart, Data: target},
				})

				if err := r.streamHandoff(ctx, em, runCtx, classifierText.String(), target, sessionID); err != nil {
					em.Error(err)
					return
				}
				continue
			}

			if err := em.Emit(ctx, f); err != nil {
				return
			}
		}

		if err := src.Err(); err != nil {
			em.Error(err)
			return
		}
		if resp := src.Response(); resp != nil {
			em.SetResponse(resp)
		}
		usage := src.Usage()
		if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.TotalTokens > 0 {
			uCopy := usage
			_ = em.Emit(ctx, niro.Frame{Kind: niro.KindUsage, Usage: &uCopy})
		}
	}()
	return out
}

// streamHandoff runs the handoff target (agent or workflow) and forwards its
// frames to em. The caller's RunContext provides messages and session.
func (r *Runner) streamHandoff(ctx context.Context, em *niro.Emitter, runCtx *RunContext, classifierReply, target, sessionID string) error {
	messages, _ := r.messagesFromRunContext(runCtx)

	handoffCtx := NewRunContext()
	handoffCtx.Set("session", map[string]any{"id": sessionID})
	handoffCtx.Set("event", map[string]any{"text": eventTextFromRunContext(runCtx)})

	if cw := r.workflows[target]; cw != nil {
		handoffCtx.Set("messages", messages)
		stream, _, _, err := cw.Run(ctx, handoffCtx, sessionID)
		if err != nil {
			return err
		}
		if stream != nil {
			return niro.Forward(ctx, stream, em)
		}
		return nil
	}

	if _, ok := r.nd.Agents[target]; ok {
		handoffCtx.Set("messages", append(append([]niro.Message(nil), messages...), niro.AssistantText(classifierReply)))
		stream, err := r.Stream(ctx, handoffCtx, target, sessionID)
		if err != nil {
			return err
		}
		return niro.Forward(ctx, stream, em)
	}

	return niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: handoff target %q not found", target)
}

const defaultMaxToolRounds = tools.DefaultMaxRounds

// shouldInclude reports whether this tool ref is included given when/unless conditions.
func (ref *AgentToolRef) shouldInclude(runCtx *RunContext) (bool, error) {
	if ref.When != "" {
		ok, err := EvalCondition(ref.When, runCtx)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	if ref.Unless != "" {
		ok, err := EvalCondition(ref.Unless, runCtx)
		if err != nil {
			return false, err
		}
		if ok {
			return false, nil
		}
	}
	return true, nil
}

// resolveAgentTools returns the tool responsibility for this agent run: filtered toolset
// (when/unless), optionally expanded descriptions, tool choice, and stream options.
func (r *Runner) resolveAgentTools(cfg *CompiledAgentConfig, runCtx *RunContext, agentName string) (AgentTools, error) {
	out := AgentTools{
		ToolChoice: cfg.ToolChoice,
		Options:    tools.DefaultToolStreamOptions(),
	}
	if cfg.Limits.MaxToolCalls > 0 {
		out.Options.MaxRounds = cfg.Limits.MaxToolCalls
	} else {
		out.Options.MaxRounds = defaultMaxToolRounds
	}

	filtered, err := r.filterTools(cfg, runCtx)
	if err != nil {
		return out, err
	}
	if toolsetNeedsExpansion(filtered) {
		expanded, err := r.toolsetWithExpandedDescriptions(filtered, runCtx)
		if err != nil {
			return out, niro.WrapError(niro.ErrCodeInvalidRequest, "tool description expansion", err)
		}
		out.Toolset = expanded
	} else {
		out.Toolset = filtered
	}
	return out, nil
}

func (r *Runner) applyToolsetTransform(agentName string, base *tools.Toolset) *tools.Toolset {
	if r.toolTransform == nil {
		return base
	}
	ts := r.toolTransform(agentName, base)
	if ts == nil {
		return tools.NewToolset()
	}
	return ts
}

func (r *Runner) mergeToolOptions(agentName string, resolved tools.ToolStreamOptions) tools.ToolStreamOptions {
	if r.toolOptions == nil {
		return resolved
	}
	opts := r.toolOptions(agentName)
	if opts.MaxRounds <= 0 {
		opts.MaxRounds = resolved.MaxRounds
	}
	if opts.ToolTimeout <= 0 {
		opts.ToolTimeout = tools.DefaultToolTimeout
	}
	return opts
}

func (r *Runner) messagesFromRunContext(runCtx *RunContext) ([]niro.Message, error) {
	if msgs, ok := GetAs[[]niro.Message](runCtx, "messages"); ok {
		return msgs, nil
	}
	return []niro.Message{niro.UserText(eventTextFromRunContext(runCtx))}, nil
}

func eventTextFromRunContext(runCtx *RunContext) string {
	ev, ok := GetAs[map[string]any](runCtx, "event")
	if !ok {
		return ""
	}
	t, _ := ev["text"].(string)
	return t
}

func (r *Runner) filterTools(cfg *CompiledAgentConfig, runCtx *RunContext) (*tools.Toolset, error) {
	out := tools.NewToolset()
	for _, ref := range cfg.ToolRefs {
		ok, err := ref.shouldInclude(runCtx)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		def, ok := r.nd.Toolset.Get(ref.Name)
		if !ok {
			continue
		}
		if err := out.Register(def); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// toolsetNeedsExpansion returns true if any tool description contains template syntax ({{).
func toolsetNeedsExpansion(ts *tools.Toolset) bool {
	for _, name := range ts.Names() {
		def, ok := ts.Get(name)
		if !ok {
			continue
		}
		if strings.Contains(def.Description, "{{") {
			return true
		}
	}
	return false
}

// toolsetWithExpandedDescriptions returns a new toolset with each tool's description
// expanded via ExpandTemplate(runCtx). All description and prompt text is template-resolved.
func (r *Runner) toolsetWithExpandedDescriptions(ts *tools.Toolset, runCtx *RunContext) (*tools.Toolset, error) {
	out := tools.NewToolset()
	for _, name := range ts.Names() {
		def, ok := ts.Get(name)
		if !ok {
			continue
		}
		expanded, err := ExpandTemplate(def.Description, runCtx)
		if err != nil {
			return nil, niro.WrapErrorf(niro.ErrCodeInvalidRequest, "tool %q description template", err, name)
		}
		withDesc, err := tools.NewToolDefinition(name, expanded, def.Schema, def.Handler)
		if err != nil {
			return nil, err
		}
		if err := out.Register(withDesc); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Run runs the agent and collects the full text response.
func (r *Runner) Run(ctx context.Context, runCtx *RunContext, agentName string, sessionID string) (string, error) {
	stream, err := r.Stream(ctx, runCtx, agentName, sessionID)
	if err != nil {
		return "", err
	}
	return niro.CollectText(ctx, stream)
}

// RunHandoff runs the handoff target (workflow or agent) from JSON and returns the reply and usage.
// Targets are resolved from workflows (BindWorkflows) then agents (NiroDefinition.Agents). history and input
// are the conversation so far and the user message; classifierReply is the classifier agent's text before handoff.
func (r *Runner) RunHandoff(ctx context.Context, history []niro.Message, input, classifierReply, handoffTarget, sessionID string) (reply string, usage niro.Usage, err error) {
	if r == nil || handoffTarget == "" {
		return "", niro.Usage{}, nil
	}
	runCtx := NewRunContext()
	runCtx.Set("session", map[string]any{"id": sessionID})
	runCtx.Set("messages", history)
	runCtx.Set("event", map[string]any{"text": input})

	if cw := r.workflows[handoffTarget]; cw != nil {
		stream, text, u, runErr := cw.Run(ctx, runCtx, sessionID)
		if runErr != nil {
			return "", u, runErr
		}
		if stream != nil {
			reply, err = niro.CollectText(ctx, stream)
			u2 := stream.Usage()
			u.InputTokens += u2.InputTokens
			u.OutputTokens += u2.OutputTokens
			u.TotalTokens += u2.TotalTokens
			return reply, u, err
		}
		return text, u, nil
	}
	if _, ok := r.nd.Agents[handoffTarget]; ok {
		handoffCtx := NewRunContext()
		handoffCtx.Set("session", map[string]any{"id": sessionID})
		handoffCtx.Set("messages", append(append([]niro.Message(nil), history...), niro.AssistantText(classifierReply)))
		handoffCtx.Set("event", map[string]any{"text": input})
		stream, streamErr := r.Stream(ctx, handoffCtx, handoffTarget, sessionID)
		if streamErr != nil {
			return "", niro.Usage{}, streamErr
		}
		reply, err = niro.CollectText(ctx, stream)
		return reply, stream.Usage(), err
	}
	return "", niro.Usage{}, niro.NewErrorf(niro.ErrCodeInvalidRequest, "dsl: handoff target %q not found", handoffTarget)
}

// Fan runs the given agents in parallel and merges their streams (orchestrate.Fan).
func (r *Runner) Fan(ctx context.Context, runCtx *RunContext, sessionID string, agentNames ...string) *niro.Stream {
	fns := make([]func(context.Context) (*niro.Stream, error), 0, len(agentNames))
	for _, name := range agentNames {
		name := name
		fns = append(fns, func(ctx context.Context) (*niro.Stream, error) {
			return r.Stream(ctx, runCtx, name, sessionID)
		})
	}
	return orchestrate.Fan(ctx, fns...)
}

// FanCollect runs the given agents in parallel and returns each agent's full text and usage.
// Used by fan_then to gather parallel results before running the synthesizer.
func (r *Runner) FanCollect(ctx context.Context, runCtx *RunContext, sessionID string, agentNames []string) (texts []string, usages []niro.Usage, err error) {
	if len(agentNames) == 0 {
		return nil, nil, nil
	}
	type result struct {
		text  string
		usage niro.Usage
		err   error
	}
	ch := make(chan result, len(agentNames))
	for _, name := range agentNames {
		name := name
		go func() {
			stream, err := r.Stream(ctx, runCtx, name, sessionID)
			if err != nil {
				ch <- result{err: err}
				return
			}
			text, collectErr := niro.CollectText(ctx, stream)
			usage := stream.Usage()
			if collectErr != nil {
				ch <- result{err: collectErr}
				return
			}
			ch <- result{text: text, usage: usage}
		}()
	}
	texts = make([]string, len(agentNames))
	usages = make([]niro.Usage, len(agentNames))
	for i := range agentNames {
		res := <-ch
		if res.err != nil {
			return nil, nil, res.err
		}
		texts[i] = res.text
		usages[i] = res.usage
	}
	return texts, usages, nil
}

// FanThen runs the given agents in parallel, collects their responses, then runs the synthesizer
// agent with those responses as context. Returns the synthesizer's stream and combined usage.
func (r *Runner) FanThen(ctx context.Context, runCtx *RunContext, sessionID string, parallelAgents []string, thenAgent string) (*niro.Stream, string, niro.Usage, error) {
	texts, usages, err := r.FanCollect(ctx, runCtx, sessionID, parallelAgents)
	if err != nil {
		return nil, "", niro.Usage{}, err
	}
	var totalUsage niro.Usage
	for _, u := range usages {
		totalUsage.InputTokens += u.InputTokens
		totalUsage.OutputTokens += u.OutputTokens
		totalUsage.TotalTokens += u.TotalTokens
	}
	messages, _ := r.messagesFromRunContext(runCtx)
	perspectiveLabels := []string{"Perspective A", "Perspective B", "Perspective C", "Perspective D"}
	for i, text := range texts {
		label := perspectiveLabel(i, perspectiveLabels)
		messages = append(messages, niro.AssistantText(fmt.Sprintf("[%s]\n%s", label, text)))
	}
	synthCtx := NewRunContext()
	for k, v := range runCtx.Snapshot() {
		synthCtx.Set(k, v)
	}
	synthCtx.Set("messages", messages)
	stream, err := r.Stream(ctx, synthCtx, thenAgent, sessionID)
	if err != nil {
		return nil, "", totalUsage, err
	}
	return stream, "", totalUsage, nil
}

// Race runs the given agents in parallel and returns the first successful text and usage.
func (r *Runner) Race(ctx context.Context, runCtx *RunContext, sessionID string, agentNames ...string) (string, niro.Usage, error) {
	fns := make([]func(context.Context) (*niro.Stream, error), 0, len(agentNames))
	for _, name := range agentNames {
		name := name
		fns = append(fns, func(ctx context.Context) (*niro.Stream, error) {
			return r.Stream(ctx, runCtx, name, sessionID)
		})
	}
	return orchestrate.Race(ctx, fns...)
}

// Sequence runs the given agents in order; each step receives the previous step's collected text as input.
func (r *Runner) Sequence(ctx context.Context, runCtx *RunContext, sessionID string, agentNames ...string) (*niro.Stream, error) {
	fns := make([]func(context.Context, string) (*niro.Stream, error), 0, len(agentNames))
	for _, name := range agentNames {
		name := name
		fns = append(fns, func(ctx context.Context, input string) (*niro.Stream, error) {
			rc := NewRunContext()
			for k, v := range runCtx.Snapshot() {
				rc.Set(k, v)
			}
			rc.Set("event", map[string]any{"text": input})
			return r.Stream(ctx, rc, name, sessionID)
		})
	}
	return orchestrate.Sequence(ctx, fns...)
}

func perspectiveLabel(index int, labels []string) string {
	if index < len(labels) {
		return labels[index]
	}
	return fmt.Sprintf("Perspective %d", index+1)
}

