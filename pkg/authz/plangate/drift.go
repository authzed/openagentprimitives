package plangate

import (
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// PhaseUsage is what one phase DECLARED against what it actually EXERCISED.
//
// This is the ratchet's input, and it is deliberately observation-only. The
// spec leaves the ratchet's trigger open — "declared but unused needs a
// concrete trigger: first call in the phase, N calls, or phase close" — and
// says slice 4 should pick the one the logging-mode data supports. That data is
// this. Building the measurement before the mechanism is the point: a ratchet
// tuned by guess would drop reach a phase was about to legitimately use, and
// the failure would look exactly like the agent misbehaving.
type PhaseUsage struct {
	// Index is the phase's position, which is its identity.
	Index int

	// Label is the phase's human-readable name, for the report. Agent-authored
	// and untrusted; display only.
	Label string

	// Entered reports whether this phase governed any call at all.
	//
	// Distinct from an empty Exercised set, and the distinction is the whole
	// signal: a phase that ran and used everything it declared and a phase that
	// never ran both exercise nothing, and only the second is over-declaration.
	Entered bool

	// Declared and Exercised are the handle sets, sorted.
	Declared  []string
	Exercised []string

	// DeclaredNeverExercised is reach this phase asked for and never used —
	// the concrete, actionable form of over-declaration, and the set a
	// use-it-or-lose-it ratchet would drop.
	DeclaredNeverExercised []string
}

// PhaseDrift computes declared-vs-exercised for every phase of a plan.
//
// PER PHASE, which the plan-wide Metric cannot be: a single number cannot tell
// an operator which phase to narrow, and the ratchet drops handles from the
// phase that did not use them. A handle declared by two phases and used by one
// is over-declaration in the other, and only a per-phase view sees that.
//
// Usage is attributed to the phase that was ACTIVE for the call — the
// PhaseIndex the gate recorded — never to whichever phase declares the handle.
// Crediting the declaring phase would report a phase as tight when it was a
// different one that exercised the reach.
//
// Records naming another plan are ignored, for the same reason Metrics ignores
// them: they are another ceiling's evidence, and blending them credits this
// plan for reach it never declared.
func PhaseDrift(plan Plan, records []plangateaudit.Content) []PhaseUsage {
	digest := plan.Digest()

	exercised := make([]map[string]struct{}, len(plan.Phases))
	entered := make([]bool, len(plan.Phases))
	for i := range exercised {
		exercised[i] = map[string]struct{}{}
	}

	for _, r := range records {
		if r.PlanDigest != digest || r.PhaseIndex == nil {
			continue
		}
		// A record with no handle is a call the gate does not govern — a meta
		// or passthrough tool. It says nothing about whether declared reach was
		// used, so it neither marks the phase entered nor counts as usage.
		if r.Handle == "" {
			continue
		}
		idx := int(*r.PhaseIndex)
		if idx < 0 || idx >= len(plan.Phases) {
			continue
		}
		entered[idx] = true
		exercised[idx][r.Handle] = struct{}{}
	}

	out := make([]PhaseUsage, 0, len(plan.Phases))
	for i, ph := range plan.Phases {
		u := PhaseUsage{Index: i, Label: ph.Label, Entered: entered[i]}

		declared := map[string]struct{}{}
		for _, h := range ph.Permissions {
			declared[h.String()] = struct{}{}
			u.Declared = append(u.Declared, h.String())
		}
		for h := range exercised[i] {
			u.Exercised = append(u.Exercised, h)
		}
		for h := range declared {
			if _, used := exercised[i][h]; !used {
				u.DeclaredNeverExercised = append(u.DeclaredNeverExercised, h)
			}
		}

		sortStrings(u.Declared)
		sortStrings(u.Exercised)
		sortStrings(u.DeclaredNeverExercised)
		out = append(out, u)
	}
	return out
}

// OverDeclared reports whether this phase asked for reach it did not use.
//
// A phase that never ran counts, and that is the intended reading: declaring a
// ceiling the session never entered is the purest over-declaration there is.
func (u PhaseUsage) OverDeclared() bool { return len(u.DeclaredNeverExercised) > 0 }
