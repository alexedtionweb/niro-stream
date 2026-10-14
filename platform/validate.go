package platform

import (
	"fmt"
	"sort"
)

var validRelationshipTypes = map[string]struct{}{
	"dependency":   {},
	"prerequisite": {},
	"continuation": {},
}

func ValidateRegistry(reg *Registry) error {
	if reg == nil {
		return fmt.Errorf("platform: registry is nil")
	}
	if reg.InitialState == "" {
		return fmt.Errorf("platform: initial state is required")
	}
	if len(reg.Domains) == 0 {
		return fmt.Errorf("platform: at least one domain is required")
	}
	if len(reg.Templates) == 0 {
		return fmt.Errorf("platform: at least one approved template is required")
	}
	if err := validateStateMachine(reg); err != nil {
		return err
	}
	if err := validateTemplates(reg.Templates); err != nil {
		return err
	}
	return validateDomains(reg)
}

func validateStateMachine(reg *Registry) error {
	if len(reg.StateMachine.States) == 0 {
		return fmt.Errorf("platform: state machine is required")
	}
	if _, ok := reg.StateMachine.States[reg.InitialState]; !ok {
		return fmt.Errorf("platform: initial state %q is missing from state machine", reg.InitialState)
	}
	for stateID, def := range reg.StateMachine.States {
		if len(def.Transitions) == 0 {
			return fmt.Errorf("platform: state %q must define at least one transition", stateID)
		}
		for _, next := range def.Transitions {
			if _, ok := reg.StateMachine.States[next]; !ok {
				return fmt.Errorf("platform: state %q references unknown transition %q", stateID, next)
			}
		}
	}
	return nil
}

func validateTemplates(templates map[string]Template) error {
	for id, t := range templates {
		if id == "" {
			return fmt.Errorf("platform: template id is required")
		}
		if t.ID != "" && t.ID != id {
			return fmt.Errorf("platform: template key %q does not match template id %q", id, t.ID)
		}
		if !t.Approved {
			return fmt.Errorf("platform: template %q must be approved", id)
		}
		if t.Text == "" {
			return fmt.Errorf("platform: template %q text is required", id)
		}
	}
	return nil
}

func validateDomains(reg *Registry) error {
	seenAliases := make(map[string]string)
	stateAllowed := allowedDomainsByState(reg.StateMachine)
	domainIDs := make([]string, 0, len(reg.Domains))
	for id := range reg.Domains {
		domainIDs = append(domainIDs, id)
	}
	sort.Strings(domainIDs)
	for _, id := range domainIDs {
		domain := reg.Domains[id]
		if domain.ID != "" && domain.ID != id {
			return fmt.Errorf("platform: domain key %q does not match domain id %q", id, domain.ID)
		}
		if len(domain.Aliases) == 0 {
			return fmt.Errorf("platform: domain %q must define aliases", id)
		}
		if len(domain.Examples) == 0 {
			return fmt.Errorf("platform: domain %q must define examples", id)
		}
		if len(domain.NegativeExamples) == 0 {
			return fmt.Errorf("platform: domain %q must define negative examples", id)
		}
		if len(domain.Capabilities) == 0 {
			return fmt.Errorf("platform: domain %q must define capabilities", id)
		}
		for _, alias := range domain.Aliases {
			key := normalizeToken(alias)
			if owner, exists := seenAliases[key]; exists {
				return fmt.Errorf("platform: alias %q conflicts between domains %q and %q", alias, owner, id)
			}
			seenAliases[key] = id
		}
		for _, rel := range domain.Relationships {
			if _, ok := reg.Domains[rel.Target]; !ok {
				return fmt.Errorf("platform: domain %q references unknown related domain %q", id, rel.Target)
			}
			if _, ok := validRelationshipTypes[rel.Type]; !ok {
				return fmt.Errorf("platform: domain %q has invalid relationship type %q", id, rel.Type)
			}
		}
		for capID, capability := range domain.Capabilities {
			if capability.ID != "" && capability.ID != capID {
				return fmt.Errorf("platform: capability key %q in domain %q does not match capability id %q", capID, id, capability.ID)
			}
			if len(capability.Scenarios) == 0 {
				return fmt.Errorf("platform: domain %q capability %q must define scenarios", id, capID)
			}
			for _, scenario := range capability.Scenarios {
				if scenario.ID == "" {
					return fmt.Errorf("platform: domain %q capability %q has scenario without id", id, capID)
				}
				if scenario.ToolID == "" {
					return fmt.Errorf("platform: scenario %q must define a tool", scenario.ID)
				}
				if scenario.NextState == "" {
					return fmt.Errorf("platform: scenario %q must define next state", scenario.ID)
				}
				if _, ok := reg.StateMachine.States[scenario.NextState]; !ok {
					return fmt.Errorf("platform: scenario %q references unknown next state %q", scenario.ID, scenario.NextState)
				}
				if scenario.ResponseTemplate == "" {
					return fmt.Errorf("platform: scenario %q must define a response template", scenario.ID)
				}
				if _, ok := reg.Templates[scenario.ResponseTemplate]; !ok {
					return fmt.Errorf("platform: scenario %q references unknown template %q", scenario.ID, scenario.ResponseTemplate)
				}
				if len(scenario.AllowedStates) == 0 {
					return fmt.Errorf("platform: scenario %q must define allowed states", scenario.ID)
				}
				for _, stateID := range scenario.AllowedStates {
					stateDef, ok := reg.StateMachine.States[stateID]
					if !ok {
						return fmt.Errorf("platform: scenario %q references unknown allowed state %q", scenario.ID, stateID)
					}
					if !contains(stateDef.AllowedDomains, id) && len(stateDef.AllowedDomains) > 0 {
						return fmt.Errorf("platform: state %q does not allow domain %q required by scenario %q", stateID, id, scenario.ID)
					}
					if allowed, ok := stateAllowed[stateID]; ok && len(allowed) > 0 && !contains(allowed, id) {
						return fmt.Errorf("platform: domain %q is not allowed in state %q", id, stateID)
					}
				}
			}
		}
	}
	return nil
}

func allowedDomainsByState(sm StateMachine) map[string][]string {
	out := make(map[string][]string, len(sm.States))
	for stateID, def := range sm.States {
		out[stateID] = normalizeStrings(def.AllowedDomains)
	}
	return out
}
