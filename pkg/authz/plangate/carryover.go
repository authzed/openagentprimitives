package plangate

import (
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// CarriesOver reports whether phase `index` of `current` is already covered by
// an approval a human gave against an EARLIER plan.
//
// Approval is keyed by PhaseRef{PlanDigest, Index}, and the digest covers every
// dimension of a phase's authority — so editing any phase re-keys the whole
// plan and nothing carries. That default is what stops a widened ceiling
// inheriting consent, and it must stay the default. But applied alone it also
// re-asks about a re-plan that only DROPPED reach, which is the fatigue this
// slice exists to remove: an agent that narrows its own plan after recon should
// not cost a human a second click for asking for less.
//
// So: the digest decides approval is gone, and this decides whether the human's
// answer still covers what is being asked. It carries only when the new phase
// widens NOTHING relative to a phase they actually approved — no added handle,
// no raised entry budget, no dropped prerequisite (see PhaseDelta.Widens).
//
// Deliberately NOT consulted here: denials. The caller checks IsDenied FIRST,
// because a denial outlives the plan it was made against — a human's "no" is
// not undone by the agent reshaping its plan, and a carry-over that could
// resurrect a refused ceiling would make re-planning a laundering step. This
// function only ever turns a "needs approval" into "already approved"; it can
// never overturn a denial, and the ordering at the call site is what guarantees
// that.
func CarriesOver(current Plan, index int, records []plangateaudit.Content) bool {
	if index < 0 || index >= len(current.Phases) {
		return false
	}
	for _, granted := range GrantedPhases(records, index, current.Digest()) {
		d, changed := DiffPhase(granted, current.Phases[index], index)
		if !changed || !d.Widens() {
			return true
		}
	}
	return false
}

// MostRecentApprovedPhase returns phase `index` as a human most recently
// GRANTED it, under some plan other than exclude.
//
// "Latest" is record order, which is the order a human answered in. A phase
// re-planned three times diffs against the most recent yes, so the card shows
// what is new since the last decision rather than since the first — an approver
// must not be re-shown additions they already cleared two rounds ago.
func MostRecentApprovedPhase(records []plangateaudit.Content, index int, exclude string) (Phase, bool) {
	granted := GrantedPhases(records, index, exclude)
	if len(granted) == 0 {
		return Phase{}, false
	}
	return granted[len(granted)-1], true
}

// GrantedPhases returns phase `index` as it was actually GRANTED under every
// plan other than exclude, in record order.
//
// The distinction this function exists to hold is the whole of partial
// approval: what a phase DECLARED and what a human CLEARED are different sets,
// and only the second confers authority. Reconstructing a prior ceiling from
// the plan_approved records — the frozen declaration — reads every held-back
// handle back as approved, so an agent could re-declare a refused handle and
// have it carry over silently. The grant is therefore read off the
// phase_approved record itself.
//
// "Cleared" means an explicit phase_approved record. A plan that was merely
// declared confers nothing — declaring a ceiling and being allowed to use it
// are the two things this feature separates.
func GrantedPhases(records []plangateaudit.Content, index int, exclude string) []Phase {
	// LAST record wins per plan, matching the fold's "the last write wins for
	// this ceiling". A second grant against the same phase is a second human
	// decision — a held-back item cleared later by its owner, or a narrower
	// re-grant — and the most recent one is the standing answer.
	type slot struct {
		phase Phase
		order int
	}
	byDigest := map[string]slot{}
	var order []string

	for _, r := range records {
		if r.Event != plangateaudit.EventPhaseApproved || r.PhaseIndex == nil {
			continue
		}
		if int(*r.PhaseIndex) != index || r.PlanDigest == "" || r.PlanDigest == exclude {
			continue
		}
		ph, ok := grantedPhase(records, r, index)
		if !ok {
			continue
		}
		if prev, dup := byDigest[r.PlanDigest]; dup {
			byDigest[r.PlanDigest] = slot{phase: ph, order: prev.order}
			continue
		}
		byDigest[r.PlanDigest] = slot{phase: ph, order: len(order)}
		order = append(order, r.PlanDigest)
	}

	out := make([]Phase, 0, len(order))
	for _, d := range order {
		out = append(out, byDigest[d].phase)
	}
	return out
}

// grantedPhase reads one approval record as the phase it granted.
//
// A record carrying no ceiling predates partial approval, when clearance was
// all-or-nothing and a phase_approved therefore meant "the whole declared
// phase". Reading it against the declaration is what it meant, and is the only
// reading that keeps an in-flight session's earlier approvals honoured across
// this change. Every record written from here on carries its subset, so the
// fallback shrinks to history rather than staying a live path.
func grantedPhase(records []plangateaudit.Content, r plangateaudit.Content, index int) (Phase, bool) {
	if len(r.Ceiling) == 0 {
		prior, ok := PlanForDigest(records, r.PlanDigest)
		if !ok || index >= len(prior.Phases) {
			return Phase{}, false
		}
		return prior.Phases[index], true
	}

	// maxCount is coerced the same way PlanForDigest coerces it: the field is
	// omitempty, so a granted budget of 1 and an absent one are the same bytes.
	maxCount := r.MaxCount
	if maxCount < 1 {
		maxCount = 1
	}
	ph := Phase{
		Consents:    r.Consents,
		Permissions: parseHandles(r.Ceiling),
		Max:         MaxSpec{Count: maxCount},
		Budget:      BudgetSpec{Calls: r.BudgetCalls},
	}
	for _, req := range r.Requires {
		ph.Requires = append(ph.Requires, RequiresEdge{Phase: req})
	}
	// With the instances the grant named, not just their types. The delta now
	// matches slots on both, so reading the granted side type-only would report
	// every named slot as an addition and re-ask about phases nothing widened.
	ph.Slots = slotsFromRecord(r)
	return ph, true
}
