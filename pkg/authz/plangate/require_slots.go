package plangate

import "fmt"

// ModeEnforcing is the plan-gate mode in which the gate actually blocks:
// it publishes cards, refuses uncovered calls, and refuses a plan that leaves a
// declared slot type unnamed.
//
// Named here rather than compared as a bare string at each site — the value is
// read in the hook, the runner and the validator, and a typo in any of them
// fails OPEN (the mode silently reads as not-enforcing, which is exactly the
// class of bug that let two live sessions run with the gate off).
const ModeEnforcing = "enforcing"

// MissingSlot is one phase that acts on a declared slot type without saying
// which instance.
type MissingSlot struct {
	PhaseIndex int
	PhaseLabel string
	// ResourceType is the slot type the phase's ceiling touches and its slots
	// do not mention.
	ResourceType string
}

// MissingSlotDeclarations reports every (phase, slot type) pair where the
// phase's ceiling names a resource type the AgentClass declares as a slot, and
// the phase says nothing at all about that type.
//
// WHY THIS IS A PRECONDITION AND NOT ADVICE. The system prompt asks the agent to
// name the resource, explains the payoff, and does not work: a live session
// recorded a prompt containing "Name the RESOURCE, not just the permission"
// verbatim, and the agent declared zero slots across three phases anyway. Every
// card could then name only a CATEGORY — "this needs perm:fetch:git_repo" — so
// the user approved a kind of action rather than a repository, and each phase
// cost its own decision. Prose is a suggestion; this is the shape requirePlan
// uses, which an agent cannot decline.
//
// SILENCE IS WHAT IS FORBIDDEN, NOT DEFERRAL. A phase that genuinely cannot know
// its target declares the type with no id and passes — the card then says the
// target is not yet named and that this phase will ask again. That escape hatch
// is load-bearing: a slot id is AUTHORITY, it enters the plan digest and bounds
// the grant, so an agent left with no legal way to say "I do not know" would
// invent one. A guessed id buys an early approval for a target nobody agreed
// to, which is strictly worse than the silence this replaces.
//
// Only types the class DECLARED participate. A phase touching a resource type
// the class never opted into has no slot to name, and demanding one would stop
// every class that has not adopted slots from planning at all.
func MissingSlotDeclarations(p Plan, declaredSlotTypes []string) []MissingSlot {
	if len(declaredSlotTypes) == 0 {
		return nil
	}
	declared := make(map[string]struct{}, len(declaredSlotTypes))
	for _, t := range declaredSlotTypes {
		declared[t] = struct{}{}
	}

	var out []MissingSlot
	for i, ph := range p.Phases {
		named := make(map[string]struct{}, len(ph.Slots))
		for _, s := range ph.Slots {
			named[s.Type] = struct{}{}
		}
		// Walked in ceiling order, deduped, so the refusal text is stable across
		// calls — the agent reads this, and an order that moves between attempts
		// reads as a different problem each time.
		seen := map[string]struct{}{}
		for _, h := range ph.Permissions {
			rt := h.ResourceType()
			if rt == "" {
				continue // a tool handle names no SpiceDB resource
			}
			if _, ok := declared[rt]; !ok {
				continue
			}
			if _, ok := named[rt]; ok {
				continue
			}
			if _, dup := seen[rt]; dup {
				continue
			}
			seen[rt] = struct{}{}
			out = append(out, MissingSlot{PhaseIndex: i, PhaseLabel: ph.Label, ResourceType: rt})
		}
	}
	return out
}

// RefusalText renders the missing declarations as the message the agent is sent
// when its plan is refused.
//
// It states both ways out, because a refusal that names only the strict one
// teaches the agent to fabricate an id to get past the gate.
func (m MissingSlot) RefusalText() string {
	return fmt.Sprintf(
		"phase %d (%q) acts on %s but names no %s. Add it to that phase's `slots` with the "+
			"resource as `id` (the user then approves that exact one, and the phase runs without "+
			"asking again). If you genuinely cannot know the target until you have looked, declare "+
			"{\"type\":%q,\"why\":\"…\"} with NO id — the phase is then marked as needing a later "+
			"approval. Do not guess an id.",
		m.PhaseIndex, m.PhaseLabel, m.ResourceType, m.ResourceType, m.ResourceType)
}
