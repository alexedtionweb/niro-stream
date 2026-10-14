package platform

import (
	"context"
	"fmt"
)

const (
	traceUserInput              = "user_input"
	traceSystemGateEvaluation   = "system_gate_evaluation"
	traceForcedInternalSteps    = "forced_internal_steps"
	traceCandidateSelection     = "candidate_selection"
	traceLLMInterpretation      = "llm_interpretation"
	traceTaskQueueUpdate        = "task_queue_update"
	traceStateMachineTransition = "state_machine_transition"
	traceScenarioSelection      = "scenario_selection"
	traceToolExecution          = "tool_execution"
	traceResponseRendering      = "response_template_rendering"
	traceUserOutput             = "user_output"
)

func (r *Runtime) Run(ctx context.Context, input TurnInput) (TurnOutput, error) {
	if r == nil {
		return TurnOutput{}, fmt.Errorf("platform: runtime is nil")
	}
	if input.SessionID == "" {
		return TurnOutput{}, fmt.Errorf("platform: session id is required")
	}
	if input.Message == "" {
		return TurnOutput{}, fmt.Errorf("platform: message is required")
	}

	state := input.State
	if state == "" {
		state = r.registry.InitialState
	}
	if _, ok := r.registry.StateMachine.States[state]; !ok {
		return TurnOutput{}, fmt.Errorf("platform: unknown state %q", state)
	}
	if input.Context == nil {
		input.Context = map[string]any{}
	}

	trace := Trace{}
	appendTrace(&trace, traceUserInput, map[string]any{
		"session_id": input.SessionID,
		"state":      state,
		"message":    input.Message,
	})

	var forced []TaskRequest
	gateSignals := make([]string, 0)
	for _, gate := range r.gates {
		result, err := gate.Evaluate(ctx, input)
		if err != nil {
			return TurnOutput{}, err
		}
		appendTrace(&trace, traceSystemGateEvaluation, map[string]any{
			"gate":         gate.Name(),
			"blocked":      result.Blocked,
			"forced_tasks": len(result.ForcedTasks),
			"signals":      append([]string(nil), result.Signals...),
		})
		if result.Blocked {
			return TurnOutput{}, fmt.Errorf("platform: gate %q blocked execution", gate.Name())
		}
		forced = append(forced, result.ForcedTasks...)
		gateSignals = append(gateSignals, result.Signals...)
	}
	appendTrace(&trace, traceForcedInternalSteps, map[string]any{
		"count": len(forced),
	})

	candidates, err := r.candidateBuilder.Build(ctx, input, r.registry)
	if err != nil {
		return TurnOutput{}, err
	}
	appendTrace(&trace, traceCandidateSelection, map[string]any{
		"domains": append([]string(nil), candidates...),
	})

	selection, err := r.interpreter.Interpret(ctx, InterpreterInput{
		Message:          input.Message,
		CandidateDomains: candidates,
		State:            state,
		Context:          cloneMap(input.Context),
	})
	if err != nil {
		return TurnOutput{}, err
	}
	appendTrace(&trace, traceLLMInterpretation, map[string]any{
		"tasks":           len(selection.Tasks),
		"primary_index":   selection.PrimaryTaskIndex,
		"filler_template": selection.FillerTemplateID,
	})

	requests := make([]TaskRequest, 0, len(forced)+len(selection.Tasks))
	requests = append(requests, forced...)
	requests = append(requests, selection.Tasks...)
	queue := make([]Task, 0, len(requests))
	for i, req := range requests {
		queue = append(queue, Task{
			ID:         r.idFactory("task", i),
			DomainID:   req.DomainID,
			Capability: req.Capability,
			Status:     TaskPending,
		})
	}
	appendTrace(&trace, traceTaskQueueUpdate, map[string]any{
		"tasks": queueSnapshot(queue),
	})

	responses := make([]RenderedResponse, 0, len(queue))
	currentState := state
	for i := range queue {
		queue[i].Status = TaskActive
		appendTrace(&trace, traceStateMachineTransition, map[string]any{
			"task_id": queue[i].ID,
			"from":    TaskPending,
			"to":      TaskActive,
			"state":   currentState,
		})

		domain, capability, err := r.lookupCapability(queue[i].DomainID, queue[i].Capability)
		if err != nil {
			return TurnOutput{}, err
		}
		if err := r.ensureDomainAllowed(currentState, domain.ID); err != nil {
			return TurnOutput{}, err
		}
		scenario, err := r.resolveScenario(capability, currentState, input.Context)
		if err != nil {
			return TurnOutput{}, err
		}
		appendTrace(&trace, traceScenarioSelection, map[string]any{
			"task_id":    queue[i].ID,
			"scenario":   scenario.ID,
			"tool_id":    scenario.ToolID,
			"next_state": scenario.NextState,
		})
		if err := r.ensureStateTransition(currentState, scenario.NextState); err != nil {
			return TurnOutput{}, err
		}

		tool, ok := r.tools[scenario.ToolID]
		if !ok {
			return TurnOutput{}, fmt.Errorf("platform: tool %q is not registered", scenario.ToolID)
		}
		result, err := tool.Execute(ctx, ToolCall{
			SessionID: input.SessionID,
			State:     currentState,
			Task:      queue[i],
			Input:     input,
			Scenario:  scenario,
		})
		if err != nil {
			return TurnOutput{}, err
		}
		appendTrace(&trace, traceToolExecution, map[string]any{
			"task_id": queue[i].ID,
			"tool_id": scenario.ToolID,
			"signals": append(append([]string(nil), gateSignals...), result.Signals...),
		})

		tmplID := scenario.ResponseTemplate
		if tmplID == "" {
			tmplID = selection.FillerTemplateID
		}
		tmpl, ok := r.registry.Templates[tmplID]
		if !ok {
			return TurnOutput{}, fmt.Errorf("platform: template %q is not registered", tmplID)
		}
		vars := cloneMap(result.Variables)
		vars["state"] = scenario.NextState
		vars["task_id"] = queue[i].ID
		text, err := r.renderer.Render(tmpl, vars)
		if err != nil {
			return TurnOutput{}, err
		}
		appendTrace(&trace, traceResponseRendering, map[string]any{
			"task_id":     queue[i].ID,
			"template_id": tmpl.ID,
		})

		responses = append(responses, RenderedResponse{
			TaskID:      queue[i].ID,
			TemplateID:  tmpl.ID,
			Text:        text,
			Variables:   vars,
			ToolSignals: append(append([]string(nil), gateSignals...), result.Signals...),
		})
		currentState = scenario.NextState
		queue[i].Status = TaskCompleted
	}

	appendTrace(&trace, traceUserOutput, map[string]any{
		"responses": len(responses),
		"state":     currentState,
	})
	return TurnOutput{
		SessionID: input.SessionID,
		State:     currentState,
		Queue:     queue,
		Responses: responses,
		Trace:     trace,
	}, nil
}

func (r *Runtime) lookupCapability(domainID, capabilityID string) (Domain, Capability, error) {
	domain, ok := r.registry.Domains[domainID]
	if !ok {
		return Domain{}, Capability{}, fmt.Errorf("platform: unknown domain %q", domainID)
	}
	capability, ok := domain.Capabilities[capabilityID]
	if !ok {
		return Domain{}, Capability{}, fmt.Errorf("platform: unknown capability %q for domain %q", capabilityID, domainID)
	}
	return domain, capability, nil
}

func (r *Runtime) resolveScenario(cap Capability, state string, ctx map[string]any) (Scenario, error) {
	for _, scenario := range cap.Scenarios {
		if !contains(scenario.AllowedStates, state) {
			continue
		}
		if scenarioMatchesContext(scenario.RequiredContext, ctx) {
			return scenario, nil
		}
	}
	return Scenario{}, fmt.Errorf("platform: no scenario matched capability %q in state %q", cap.ID, state)
}

func (r *Runtime) ensureDomainAllowed(state, domainID string) error {
	def := r.registry.StateMachine.States[state]
	if len(def.AllowedDomains) == 0 {
		return nil
	}
	if !contains(def.AllowedDomains, domainID) {
		return fmt.Errorf("platform: domain %q is not allowed in state %q", domainID, state)
	}
	return nil
}

func (r *Runtime) ensureStateTransition(from, to string) error {
	def := r.registry.StateMachine.States[from]
	if !contains(def.Transitions, to) {
		return fmt.Errorf("platform: invalid state transition from %q to %q", from, to)
	}
	return nil
}

func scenarioMatchesContext(required map[string]string, values map[string]any) bool {
	if len(required) == 0 {
		return true
	}
	for key, expected := range required {
		value, ok := values[key]
		if !ok {
			return false
		}
		if fmt.Sprint(value) != expected {
			return false
		}
	}
	return true
}

func queueSnapshot(queue []Task) []map[string]any {
	out := make([]map[string]any, 0, len(queue))
	for _, task := range queue {
		out = append(out, map[string]any{
			"id":         task.ID,
			"domain_id":  task.DomainID,
			"capability": task.Capability,
			"status":     task.Status,
		})
	}
	return out
}

func appendTrace(trace *Trace, name string, details map[string]any) {
	trace.Steps = append(trace.Steps, TraceStep{Name: name, Details: details})
}

func cloneMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
