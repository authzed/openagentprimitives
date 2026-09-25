package plangate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// planApprovedRecords is what the runner writes when a plan is frozen: one
// record per phase, carrying that phase's full authority.
//
// Built from the same projection production uses. Assembled field by field
// here, the fixture forgets whatever the writer forgets — which is how a slot's
// instance stayed out of both for a whole release.
func planApprovedRecords(p plangate.Plan) []plangateaudit.Content {
	var out []plangateaudit.Content
	for i := range p.Phases {
		rec := plangate.PhaseAuthorityRecord(p, i, nil)
		rec.Event = plangateaudit.EventPlanApproved
		rec.PlanDigest = p.Digest()
		out = append(out, rec)
	}
	return out
}

func phaseApproved(p plangate.Plan, index int) plangateaudit.Content {
	idx := int32(index)
	return plangateaudit.Content{
		Event:      plangateaudit.EventPhaseApproved,
		PlanDigest: p.Digest(),
		PhaseIndex: &idx,
	}
}

// phaseApprovedSubset is what a PARTIAL approval writes: the human cleared only
// some of what the phase declared, so the record carries the approved subset
// rather than the requested one.
func phaseApprovedSubset(
	t *testing.T, p plangate.Plan, index int, perms []string, max int, requires ...int,
) plangateaudit.Content {
	t.Helper()
	idx := int32(index)
	ceiling := make([]string, 0, len(perms))
	for _, h := range hs(t, perms...) {
		ceiling = append(ceiling, h.String())
	}
	return plangateaudit.Content{
		Event:      plangateaudit.EventPhaseApproved,
		PlanDigest: p.Digest(),
		PhaseIndex: &idx,
		Ceiling:    ceiling,
		MaxCount:   max,
		Requires:   requires,
	}
}

func phaseOf(t *testing.T, perms []string, max int, requires ...int) plangate.Phase {
	t.Helper()
	ph := plangate.Phase{Permissions: hs(t, perms...), Max: plangate.MaxSpec{Count: max}}
	for _, r := range requires {
		ph.Requires = append(ph.Requires, plangate.RequiresEdge{Phase: r})
	}
	return ph
}

// The fatigue fix: an agent that narrows its own plan after recon must not cost
// a human a second click for asking for LESS.
func TestCarriesOver_NarrowedReplanKeepsTheApproval(t *testing.T) {
	approved := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc", "perm:write:doc"}, 2),
	}}
	narrowed := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc"}, 1),
	}}

	records := append(planApprovedRecords(approved), phaseApproved(approved, 0))

	assert.True(t, plangate.CarriesOver(narrowed, 0, records))
}

// The security case, and the enumeration of what authority IS: one row per
// dimension the plan digest covers, each blocking carry-over on its own — a
// widening with no new handle in it is still a widening. A dimension added to
// the digest without a row here is a hole in the direction nobody notices,
// because the gate keeps working and simply stops asking.
//
// The instance axis needs a granted slot to move, so it cannot share this
// baseline; it is enumerated in TestCarriesOver_ADifferentInstanceMustReachAHuman.
func TestCarriesOver_AnyWideningBlocksIt(t *testing.T) {
	approved := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc"}, 1, 0),
	}}
	records := append(planApprovedRecords(approved), phaseApproved(approved, 0))

	cases := []struct {
		name  string
		phase plangate.Phase
	}{
		{"an added handle", phaseOf(t, []string{"perm:read:doc", "perm:write:doc"}, 1, 0)},
		{"a raised entry budget", phaseOf(t, []string{"perm:read:doc"}, 9, 0)},
		{"a dropped prerequisite", phaseOf(t, []string{"perm:read:doc"}, 1)},
		{"a newly requested slot", withSlots(phaseOf(t, []string{"perm:read:doc"}, 1, 0),
			plangate.Slot{Type: "crm_company", ID: "4210"})},
	}
	for _, tc := range cases {
		t.Run(tc.name+" must still reach a human", func(t *testing.T) {
			widened := plangate.Plan{Phases: []plangate.Phase{tc.phase}}
			assert.False(t, plangate.CarriesOver(widened, 0, records))
		})
	}
}

// Partial approval's central risk. When a human clears only PART of a declared
// phase, carry-over must judge the re-plan against what they GRANTED, never
// against what the agent asked for.
//
// Reconstructing the prior ceiling from the plan_approved records — the frozen
// DECLARATION — reads the held-back handle back as approved, so re-declaring it
// widens nothing and sails through. That is a silent widening: the human said no
// to `write` and the agent gets it by re-planning.
func TestCarriesOver_APartialApprovalGrantsOnlyItsApprovedSubset(t *testing.T) {
	// Three handles declared, so a re-plan can re-ask for the held-back one and
	// still digest differently from the declaration — otherwise the same-digest
	// exclude answers the question and this proves nothing.
	declared := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc", "perm:write:doc", "perm:list:doc"}, 1),
	}}
	// The human cleared the read and held back the write.
	records := append(planApprovedRecords(declared),
		phaseApprovedSubset(t, declared, 0, []string{"perm:read:doc"}, 1))

	reAsking := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc", "perm:write:doc"}, 1),
	}}
	assert.False(t, plangate.CarriesOver(reAsking, 0, records),
		"perm:write:doc was held back; re-declaring it must reach the human again")

	withinGrant := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc"}, 1),
	}}
	assert.True(t, plangate.CarriesOver(withinGrant, 0, records),
		"a re-plan confined to the approved subset costs nobody a second click")
}

// The other two widening dimensions have to come off the approved subset too. A
// partial approval that also trimmed the entry budget or kept a prerequisite
// must not have the declaration's looser values read back in its place.
func TestCarriesOver_APartialApprovalBoundsBudgetAndPrerequisites(t *testing.T) {
	declared := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc"}, 9), // asked for nine entries, no prerequisite
	}}
	// Cleared for one entry, and only after phase 0 has run.
	records := append(planApprovedRecords(declared),
		phaseApprovedSubset(t, declared, 0, []string{"perm:read:doc"}, 1, 0))

	cases := []struct {
		name  string
		phase plangate.Phase
	}{
		{"re-raising the entry budget", phaseOf(t, []string{"perm:read:doc"}, 9, 0)},
		{"dropping the prerequisite", phaseOf(t, []string{"perm:read:doc"}, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name+" must still reach a human", func(t *testing.T) {
			assert.False(t, plangate.CarriesOver(
				plangate.Plan{Phases: []plangate.Phase{tc.phase}}, 0, records))
		})
	}
}

// Records written before partial approval existed carry no subset, and their
// meaning is unambiguous: approval was all-or-nothing, so the whole declared
// phase was cleared. Reading them against the declaration is what they meant.
func TestCarriesOver_AnApprovalWithNoRecordedSubsetMeansTheWholePhase(t *testing.T) {
	approved := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc", "perm:write:doc"}, 2),
	}}
	records := append(planApprovedRecords(approved), phaseApproved(approved, 0))

	narrowed := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc"}, 1),
	}}
	assert.True(t, plangate.CarriesOver(narrowed, 0, records))
}

// Declaring a ceiling and being cleared to use it are the two things this
// feature separates. A plan that was frozen but never approved confers nothing
// on a later plan, however much it contained.
func TestCarriesOver_ADeclaredButUnapprovedPlanConfersNothing(t *testing.T) {
	declared := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc", "perm:write:doc"}, 5),
	}}
	narrowed := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc"}, 1),
	}}

	// planApprovedRecords only: the plan was frozen, nobody cleared the phase.
	records := planApprovedRecords(declared)

	assert.False(t, plangate.CarriesOver(narrowed, 0, records),
		"a frozen plan is not an approved one")
}

// Approval is per phase INDEX. Clearing phase 0 says nothing about phase 1,
// even when phase 1 asks for less than phase 0 held.
func TestCarriesOver_ApprovalDoesNotSpreadAcrossPhases(t *testing.T) {
	approved := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc", "perm:write:doc"}, 3),
		phaseOf(t, []string{"perm:read:issue"}, 1),
	}}
	// Only phase 0 was cleared.
	records := append(planApprovedRecords(approved), phaseApproved(approved, 0))

	next := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc"}, 1),
		phaseOf(t, []string{"perm:read:doc"}, 1), // subset of phase 0's ceiling
	}}

	assert.True(t, plangate.CarriesOver(next, 0, records), "phase 0 was cleared")
	assert.False(t, plangate.CarriesOver(next, 1, records),
		"phase 1 was never cleared; borrowing phase 0's answer would be the "+
			"index substitution PhaseRef exists to prevent")
}

// A phase the earlier plan did not have is entirely new authority.
func TestCarriesOver_APhaseBeyondThePriorPlanIsNew(t *testing.T) {
	approved := plangate.Plan{Phases: []plangate.Phase{phaseOf(t, []string{"perm:read:doc"}, 1)}}
	records := append(planApprovedRecords(approved), phaseApproved(approved, 0),
		phaseApproved(approved, 1)) // a stray approval for an index the plan lacks

	extended := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc"}, 1),
		phaseOf(t, []string{"perm:write:issue"}, 1),
	}}

	assert.False(t, plangate.CarriesOver(extended, 1, records))
}

// Out-of-range indices are not a question anyone should be asked.
func TestCarriesOver_OutOfRangeIndexIsFalse(t *testing.T) {
	p := plangate.Plan{Phases: []plangate.Phase{phaseOf(t, []string{"perm:read:doc"}, 1)}}
	records := append(planApprovedRecords(p), phaseApproved(p, 0))

	assert.False(t, plangate.CarriesOver(p, -1, records))
	assert.False(t, plangate.CarriesOver(p, 7, records))
}

// Sanity: the reconstruction the whole rule rests on round-trips a plan's
// authority, so a plan that was approved carries over to an identical re-plan.
func TestPlanForDigest_RoundTripsAuthority(t *testing.T) {
	p := plangate.Plan{Phases: []plangate.Phase{
		phaseOf(t, []string{"perm:read:doc", "perm:write:doc"}, 3, 0),
	}}

	got, ok := plangate.PlanForDigest(planApprovedRecords(p), p.Digest())
	require.True(t, ok)
	assert.Equal(t, p.Digest(), got.Digest(),
		"a rebuilt plan must digest identically or every fold discards its records")
	assert.Equal(t, hs(t, "perm:read:doc", "perm:write:doc"), sortedOf(got.Phases[0].Permissions))
}

func sortedOf(in []permsurface.Handle) []permsurface.Handle {
	out := append([]permsurface.Handle(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].String() < out[i].String() {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// withSlots names the instances a phase will act on. Composed onto phaseOf
// rather than duplicating it, so a slot case can still carry prerequisites.
func withSlots(ph plangate.Phase, slots ...plangate.Slot) plangate.Phase {
	ph.Slots = slots
	return ph
}

// phaseApprovedInFull is what the runner writes on a yes: the projection of the
// phase it cleared, stamped as an approval. Built from PhaseAuthorityRecord
// rather than by hand, because a fixture that assembles the record itself
// cannot catch a field production forgets.
func phaseApprovedInFull(p plangate.Plan, index int) plangateaudit.Content {
	rec := plangate.PhaseAuthorityRecord(p, index, nil)
	rec.Event = plangateaudit.EventPhaseApproved
	rec.PlanDigest = p.Digest()
	return rec
}

// THE instance-axis security case, and the one the other widening tests cannot
// see: a slot's id is authority — it enters the plan digest and bounds the
// grant — so re-pointing a phase at a DIFFERENT instance is a widening however
// little else moved.
//
// Carry-over is a second route to approval, taken precisely when the digest no
// longer matches. Compare slots by type alone and an approval for one company
// silently covers the next one the agent names, which is the exact escape the
// instance axis exists to close.
// Every case NARROWS the handle set identically, so the only thing that moves
// between them is the instance. A plan that changed nothing would digest the
// same and never reach carry-over at all — the same-digest exclude answers that
// one, and testing it here would prove nothing about slots.
func TestCarriesOver_ADifferentInstanceMustReachAHuman(t *testing.T) {
	declared := []string{"perm:read:doc", "perm:write:doc"}
	narrowed := []string{"perm:read:doc"}
	company := func(id string) plangate.Slot { return plangate.Slot{Type: "crm_company", ID: id} }
	planOf := func(perms []string, s plangate.Slot) plangate.Plan {
		return plangate.Plan{Phases: []plangate.Phase{withSlots(phaseOf(t, perms, 1), s)}}
	}

	approved := planOf(declared, company("4210"))
	records := append(planApprovedRecords(approved), phaseApprovedInFull(approved, 0))

	assert.True(t, plangate.CarriesOver(planOf(narrowed, company("4210")), 0, records),
		"asking for less against the SAME instance is what carry-over is for")
	assert.False(t, plangate.CarriesOver(planOf(narrowed, company("4299")), 0, records),
		"an approval for one company must not be spendable on another")

	// The deferred→named direction: clearing a phase that named no target
	// cannot pre-clear whichever target it later picks.
	deferred := planOf(declared, company(""))
	deferredRecords := append(planApprovedRecords(deferred), phaseApprovedInFull(deferred, 0))
	assert.False(t, plangate.CarriesOver(planOf(narrowed, company("4210")), 0, deferredRecords),
		"an unnamed target names nothing, so it cannot cover a named one")
}
