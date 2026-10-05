package plangate

import (
	"reflect"
	"sort"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

// PhaseDelta is what one phase of a re-plan asks for beyond what a human
// already approved.
//
// Only Added is authorization-relevant. Removed is carried for the card's
// narrative — an approver reading "adds X, drops Y" is judging a different
// proposal than one reading "adds X" — but dropping reach can never need
// consent, so it never gates.
type PhaseDelta struct {
	ConsentsChanged bool
	// Index is the phase's position in the NEW plan. Identity is positional
	// (PhaseRef{digest,index}), never the agent's phase name.
	Index int

	// Label is the new phase's label, for display only.
	Label string

	// Added are handles in the new ceiling that the approved one did not hold.
	// Sorted, so the card and the audit record are stable across runs.
	Added []permsurface.Handle

	// Removed are handles the approved ceiling held and the new one drops.
	Removed []permsurface.Handle

	// MaxWas / MaxNow are the phase's entry budget before and after. A RAISE is
	// a widening with no new handle in it: the same ceiling, exercised more
	// times. Equal values mean untouched.
	MaxWas int
	MaxNow int

	// RequiresDropped are prerequisite phases the approved plan required and
	// the new one does not.
	//
	// Dropping an ordering edge widens. It is the one case where REMOVING
	// something grants authority: a phase gated behind "recon must have run"
	// becomes enterable immediately, so the agent reaches the same handles at a
	// point in the session the approver did not agree to. pkg/authz/plangate's
	// freeze step says the same thing about why it may not discard edges.
	RequiresDropped []int

	// SlotsAdded are resource types the new phase asks to touch that the granted
	// one did not.
	//
	// A slot request is the instance axis's authority: approving a plan that
	// carries one WRITES a SpiceDB grant. So adding a slot widens even when the
	// permission ceiling, the budget and the ordering are all untouched — the
	// same shape as a raised Max, and the reason Widens() cannot be a handle-set
	// comparison.
	SlotsAdded []Slot

	// SlotsRemoved are slot requests the granted phase held and the new one
	// drops. Carried for the card's narrative only; asking for fewer grants can
	// never need consent.
	SlotsRemoved []Slot

	// BudgetWas / BudgetNow are the phase's governed-call budget before and
	// after. A RAISE widens with no new reach in it at all: the same ceiling,
	// exercised longer. Zero on either side means unbounded, which is why the
	// comparison lives in Widens rather than being a bare >.
	BudgetWas int
	BudgetNow int
}

// Widens reports whether this phase asks for authority it was not granted.
//
// EVERY dimension the plan digest covers, not just the handle set: a re-plan
// can widen without adding a single handle — by raising the entry budget, by
// dropping a prerequisite, or by requesting a slot whose approval writes a
// SpiceDB grant. A caller that skips the human on !Widens() waves through
// whichever dimension is missing here, silently.
//
// Keep this in lockstep with Plan.Digest: the digest decides that approval is
// GONE, and this decides whether the human's earlier answer still covers what
// is being asked. A dimension in one and not the other is a hole in exactly one
// direction — and it is the direction nobody notices, because the gate keeps
// working and simply stops asking.
func (d PhaseDelta) Widens() bool {
	return d.ConsentsChanged || len(d.Added) > 0 ||
		d.MaxNow > d.MaxWas ||
		len(d.RequiresDropped) > 0 ||
		len(d.SlotsAdded) > 0 ||
		d.budgetRaised()
}

// budgetRaised reports whether the call budget grew.
//
// Zero is UNBOUNDED, so the comparison cannot be a bare >: going from a
// declared 5 to an undeclared 0 is a move to unbounded, which is the largest
// widening available and would read as a narrowing under numeric comparison.
func (d PhaseDelta) budgetRaised() bool {
	if d.BudgetWas == d.BudgetNow {
		return false
	}
	if d.BudgetNow == 0 {
		return true // unbounded now; bounded before
	}
	if d.BudgetWas == 0 {
		return false // was unbounded; any bound is a narrowing
	}
	return d.BudgetNow > d.BudgetWas
}

// PlanDelta is the whole re-plan's difference from the approved plan.
type PlanDelta struct {
	Phases []PhaseDelta
}

// Widens reports whether ANY phase asks for new reach — the question that
// decides whether a human is asked at all.
func (d PlanDelta) Widens() bool {
	for _, p := range d.Phases {
		if p.Widens() {
			return true
		}
	}
	return false
}

// AddedHandles is every added handle across the plan, deduped and sorted. The
// count is what makes "one card, not N" checkable.
func (d PlanDelta) AddedHandles() []permsurface.Handle {
	seen := map[permsurface.Handle]struct{}{}
	for _, p := range d.Phases {
		for _, h := range p.Added {
			seen[h] = struct{}{}
		}
	}
	return sortedHandles(seen)
}

// DiffPlans computes what newPlan asks for beyond approved.
//
// This exists because a re-plan invalidates approval STRUCTURALLY, not
// selectively: a phase is cleared by PhaseRef{PlanDigest, Index}, and the digest
// covers the permissions, so editing any phase re-keys every phase in the plan.
// That is the right default for authorization — it is why a widened ceiling
// cannot inherit consent — but on its own it also revokes approval for a plan
// that merely DROPPED reach, and would re-ask about phases nobody touched.
//
// So the digest decides that approval is gone; this decides what to ask for.
// A phase whose Added set is empty is not a question for a human: it holds
// nothing the approver did not already agree to, whatever else changed about it.
//
// Phases are matched BY INDEX, which is the same identity authorization uses.
// Matching by label would let an agent rename a phase to inherit a different
// phase's approval — the exact substitution PhaseRef exists to prevent. A phase
// beyond the approved plan's length is new, so all of its ceiling is Added.
func DiffPlans(approved, newPlan Plan) PlanDelta {
	out := PlanDelta{}
	for i, ph := range newPlan.Phases {
		var prior Phase
		if i < len(approved.Phases) {
			prior = approved.Phases[i]
		}
		// A phase beyond the approved plan's length diffs against the zero
		// Phase, whose empty ceiling makes all of its reach Added.
		if d, changed := DiffPhase(prior, ph, i); changed {
			out.Phases = append(out.Phases, d)
		}
	}
	return out
}

// DiffPhase computes one phase's delta against the version of it that was
// granted. changed=false means the phase is untouched in every dimension that
// carries authority, which is the strongest possible "this needs no consent".
//
// Exported and shared because carry-over asks the same question about a single
// phase that DiffPlans asks about all of them, and the two must not drift: every
// widening dimension lives in exactly one place (see PhaseDelta.Widens), so a
// new one can never be added to the card's diff and forgotten by the gate's.
func DiffPhase(granted, current Phase, index int) (PhaseDelta, bool) {
	grantedSet := handleSet(granted.Permissions)
	currentSet := handleSet(current.Permissions)

	added := map[permsurface.Handle]struct{}{}
	for h := range currentSet {
		if _, ok := grantedSet[h]; !ok {
			added[h] = struct{}{}
		}
	}
	removed := map[permsurface.Handle]struct{}{}
	for h := range grantedSet {
		if _, ok := currentSet[h]; !ok {
			removed[h] = struct{}{}
		}
	}
	dropped := droppedRequires(granted.Requires, current.Requires)
	slotsAdded, slotsRemoved := diffSlots(granted.Slots, current.Slots)
	budgetChanged := granted.Budget.Calls != current.Budget.Calls
	consentsChanged := !reflect.DeepEqual(granted.Consents, current.Consents)

	if len(added) == 0 && len(removed) == 0 &&
		granted.Max.Count == current.Max.Count && len(dropped) == 0 &&
		len(slotsAdded) == 0 && len(slotsRemoved) == 0 && !budgetChanged && !consentsChanged {
		return PhaseDelta{}, false
	}
	return PhaseDelta{
		ConsentsChanged: consentsChanged,
		Index:           index,
		Label:           current.Label,
		Added:           sortedHandles(added),
		Removed:         sortedHandles(removed),
		MaxWas:          granted.Max.Count,
		MaxNow:          current.Max.Count,
		RequiresDropped: dropped,
		SlotsAdded:      slotsAdded,
		SlotsRemoved:    slotsRemoved,
		BudgetWas:       granted.Budget.Calls,
		BudgetNow:       current.Budget.Calls,
	}, true
}

// diffSlots returns the slot requests added and removed, each sorted so a card
// and an audit record read the same way across runs.
// Matched on TYPE AND INSTANCE, because both are authority. A slot's id enters
// the plan digest and bounds the grant, so a phase re-pointed from one company
// to another has been widened — and matching on type alone reported no
// difference at all, which let carry-over spend an approval for one instance on
// the next one the agent named. Keep this in lockstep with Phase.AuthorityKey,
// which hashes the same pair.
//
// A phase that named no target and then names one is likewise an addition: an
// unnamed slot covers no instance, so the named one is new authority.
func diffSlots(granted, current []Slot) (added, removed []Slot) {
	key := func(s Slot) string { return s.Type + "\x1f" + s.ID }
	has := func(in []Slot, k string) bool {
		for _, s := range in {
			if key(s) == k {
				return true
			}
		}
		return false
	}
	for _, s := range current {
		if !has(granted, key(s)) && !has(added, key(s)) {
			added = append(added, s)
		}
	}
	for _, s := range granted {
		if !has(current, key(s)) && !has(removed, key(s)) {
			removed = append(removed, s)
		}
	}
	byTypeThenID := func(in []Slot) func(i, j int) bool {
		return func(i, j int) bool {
			if in[i].Type != in[j].Type {
				return in[i].Type < in[j].Type
			}
			return in[i].ID < in[j].ID
		}
	}
	sort.Slice(added, byTypeThenID(added))
	sort.Slice(removed, byTypeThenID(removed))
	return added, removed
}

// droppedRequires returns the prerequisite phase indices present in prior and
// absent from current, sorted. Edge `Why` is agent narration and is ignored:
// re-wording a justification does not change what the edge constrains.
func droppedRequires(prior, current []RequiresEdge) []int {
	if len(prior) == 0 {
		return nil
	}
	have := make(map[int]struct{}, len(current))
	for _, e := range current {
		have[e.Phase] = struct{}{}
	}
	var out []int
	for _, e := range prior {
		if _, ok := have[e.Phase]; !ok {
			out = append(out, e.Phase)
		}
	}
	sort.Ints(out)
	return out
}

func handleSet(hs []permsurface.Handle) map[permsurface.Handle]struct{} {
	out := make(map[permsurface.Handle]struct{}, len(hs))
	for _, h := range hs {
		out[h] = struct{}{}
	}
	return out
}

func sortedHandles(set map[permsurface.Handle]struct{}) []permsurface.Handle {
	if len(set) == 0 {
		return nil
	}
	out := make([]permsurface.Handle, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}
