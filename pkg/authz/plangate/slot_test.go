package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// A slot request is AUTHORITY, not narration: approving a plan that requests a
// slot writes the grant that makes the instance axis pass. A digest that did
// not cover slots would let an agent add one to an already-approved plan and
// present an unchanged digest — carry-over would hold, and nobody would be
// asked about a grant that is about to be written.
func TestDigest_coversSlotRequests(t *testing.T) {
	base := []permsurface.Handle{handle(t, "read", "doc")}
	without := Plan{Phases: []Phase{{Permissions: base, Max: MaxSpec{Count: 1}}}}
	with := Plan{Phases: []Phase{{
		Permissions: base,
		Max:         MaxSpec{Count: 1},
		Slots:       []Slot{{Type: "crm_company"}},
	}}}

	assert.NotEqual(t, without.Digest(), with.Digest(),
		"requesting a slot changes what approving the plan will grant")
}

// A slot set is a SET, like the permission ceiling: re-declaring the same slots
// in a different order requests identical authority and must digest
// identically, or a cosmetic reorder would revoke an approval.
func TestDigest_slotOrderDoesNotMatter(t *testing.T) {
	a := Plan{Phases: []Phase{{Max: MaxSpec{Count: 1}, Slots: []Slot{
		{Type: "crm_company"}, {Type: "crm_contact"},
	}}}}
	b := Plan{Phases: []Phase{{Max: MaxSpec{Count: 1}, Slots: []Slot{
		{Type: "crm_contact"}, {Type: "crm_company"},
	}}}}

	assert.Equal(t, a.Digest(), b.Digest())
}

// The agent's reason for wanting a slot is narration, and narration never
// re-keys a plan — the same rule every other Why follows.
func TestFreezeFrom_slotWhyIsNarrationAndNeverReachesTheDigest(t *testing.T) {
	surface := surfaceWith(t, "perm:read:tracker_issue")
	build := func(why string) Plan {
		p, probs := FreezeFrom([]AuthoredPhase{{
			ID: "recon", Label: "Recon", Why: "read",
			Permissions: []AuthoredPermission{{Handle: "perm:read:tracker_issue", Why: "to read"}},
			Slots:       []AuthoredSlot{{Type: "crm_company", Why: why}},
		}}, surface, []string{"crm_company"})
		require.Empty(t, probs)
		return p
	}

	assert.Equal(t, build("because I must").Digest(), build("totally different words").Digest())
}

// A slot type the AgentClass never declared is not authority the agent may
// request — it is a typo or an invention. Dropped loudly, exactly as an unknown
// permission handle is, rather than carried into a plan a human is then asked
// to approve.
func TestFreezeFrom_anUndeclaredSlotTypeIsDroppedNotCarried(t *testing.T) {
	surface := surfaceWith(t, "perm:read:tracker_issue")
	p, probs := FreezeFrom([]AuthoredPhase{{
		ID: "recon", Label: "Recon", Why: "read",
		Permissions: []AuthoredPermission{{Handle: "perm:read:tracker_issue", Why: "to read"}},
		Slots: []AuthoredSlot{
			{Type: "crm_company", Why: "declared by the class"},
			{Type: "nuclear_launch", Why: "invented"},
		},
	}}, surface, []string{"crm_company"})

	require.Len(t, p.Phases, 1)
	assert.Equal(t, []Slot{{Type: "crm_company", Why: "declared by the class"}}, p.Phases[0].Slots)

	require.Len(t, probs, 1, "a dropped slot must be reported, never silent")
	assert.Contains(t, probs[0].Detail, "nuclear_launch")
}

// With no declared slot types the class does not use the instance axis at all,
// so every request is undeclared. Failing closed matters here: admitting them
// would let a class that never opted in have grants written against it.
func TestFreezeFrom_withNoDeclaredSlotTypesEveryRequestIsDropped(t *testing.T) {
	surface := surfaceWith(t, "perm:read:tracker_issue")
	p, probs := FreezeFrom([]AuthoredPhase{{
		ID: "recon", Label: "Recon", Why: "read",
		Permissions: []AuthoredPermission{{Handle: "perm:read:tracker_issue", Why: "to read"}},
		Slots:       []AuthoredSlot{{Type: "crm_company", Why: "want it"}},
	}}, surface, nil)

	require.Len(t, p.Phases, 1)
	assert.Empty(t, p.Phases[0].Slots)
	assert.Len(t, probs, 1)
}

// Slots are deduplicated and ordered on freeze, so the frozen plan is a pure
// function of what was requested — the same property that makes an identical
// re-submit digest identically.
func TestFreezeFrom_slotsAreDedupedAndOrdered(t *testing.T) {
	surface := surfaceWith(t, "perm:read:tracker_issue")
	p, probs := FreezeFrom([]AuthoredPhase{{
		ID: "recon", Label: "Recon", Why: "read",
		Permissions: []AuthoredPermission{{Handle: "perm:read:tracker_issue", Why: "to read"}},
		Slots: []AuthoredSlot{
			{Type: "crm_contact", Why: "second"},
			{Type: "crm_company", Why: "first"},
			{Type: "crm_contact", Why: "again"},
		},
	}}, surface, []string{"crm_company", "crm_contact"})

	require.Empty(t, probs)
	// Why rides through freeze for the card, and dedup keeps the FIRST
	// occurrence — so crm_contact carries "second" (the one that was seen) and
	// not "again" (the duplicate that was dropped).
	assert.Equal(t, []Slot{
		{Type: "crm_company", Why: "first"},
		{Type: "crm_contact", Why: "second"},
	}, p.Phases[0].Slots)
}

// Slots are in the digest, so a plan rebuilt from the log must carry them or it
// digests differently from the plan the records name — and a fold against it
// discards every one of those records as belonging to another plan.
//
// This is the coupling that makes the audit record's shape load-bearing rather
// than cosmetic: adding a dimension to the digest without adding it to the
// record silently breaks reconstruction everywhere.
func TestPlanForDigest_roundTripsSlotRequests(t *testing.T) {
	p := Plan{Phases: []Phase{{
		Permissions: []permsurface.Handle{handle(t, "read", "doc")},
		Max:         MaxSpec{Count: 1},
		Slots:       []Slot{{Type: "crm_company"}, {Type: "crm_contact"}},
	}}}

	got, ok := PlanForDigest(planApprovedRecordsFor(p), p.Digest())
	require.True(t, ok)
	assert.Equal(t, p.Digest(), got.Digest(),
		"a rebuilt plan must digest identically or every fold discards its records")
	assert.Equal(t, p.Phases[0].Slots, got.Phases[0].Slots)
}

// Requesting a slot the approved phase did not hold is a WIDENING — approving
// it writes a SpiceDB grant that did not exist before. It is the fourth
// dimension the digest covers, and Widens() is what every caller consults to
// decide whether a human is asked at all.
func TestDiffPhase_requestingANewSlotWidens(t *testing.T) {
	granted := Phase{
		Permissions: []permsurface.Handle{handle(t, "read", "doc")},
		Max:         MaxSpec{Count: 1},
	}
	asking := Phase{
		Permissions: []permsurface.Handle{handle(t, "read", "doc")},
		Max:         MaxSpec{Count: 1},
		Slots:       []Slot{{Type: "crm_company"}},
	}

	d, changed := DiffPhase(granted, asking, 0)
	require.True(t, changed)
	assert.Equal(t, []Slot{{Type: "crm_company"}}, d.SlotsAdded)
	assert.True(t, d.Widens(),
		"a new slot request is authority the approver never granted")
}

// Dropping a slot request asks for strictly less, so it cannot need consent —
// the same rule that lets a narrowed permission set carry its approval forward.
func TestDiffPhase_droppingASlotDoesNotWiden(t *testing.T) {
	granted := Phase{Max: MaxSpec{Count: 1}, Slots: []Slot{{Type: "crm_company"}}}
	asking := Phase{Max: MaxSpec{Count: 1}}

	d, changed := DiffPhase(granted, asking, 0)
	require.True(t, changed, "the phase did change; it just changed in a safe direction")
	assert.False(t, d.Widens())
}

// planApprovedRecordsFor is what the runner writes when a plan freezes.
func planApprovedRecordsFor(p Plan) []plangateaudit.Content {
	var out []plangateaudit.Content
	for i, ph := range p.Phases {
		idx := int32(i)
		ceiling := make([]string, 0, len(ph.Permissions))
		for _, h := range ph.Permissions {
			ceiling = append(ceiling, h.String())
		}
		slots := make([]string, 0, len(ph.Slots))
		for _, s := range ph.Slots {
			slots = append(slots, s.Type)
		}
		var requires []int
		for _, e := range ph.Requires {
			requires = append(requires, e.Phase)
		}
		out = append(out, plangateaudit.Content{
			Event:       plangateaudit.EventPlanApproved,
			PlanDigest:  p.Digest(),
			PhaseIndex:  &idx,
			Ceiling:     ceiling,
			Slots:       slots,
			MaxCount:    ph.Max.Count,
			BudgetCalls: ph.Budget.Calls,
			Requires:    requires,
		})
	}
	return out
}

// The safety property that has to be in place BEFORE approving a plan writes a
// grant: a phase requesting a slot it does not already hold can never
// auto-approve.
//
// Tier 0 exists so an all-readonly recon phase costs a human nothing. But a
// slot request is not readonly in effect — approving it MINTS a SpiceDB grant
// on somebody's resource. Pricing it by handle severity alone would let the
// cheapest possible phase (all-readonly, inside the budget) silently acquire an
// instance grant with nobody asked, which is the one outcome the whole gate
// exists to prevent.
func TestComputeTier_anUngrantedSlotRequestCannotAutoApprove(t *testing.T) {
	readonly := []HandleImpact{{Handle: handle(t, "read", "doc"), StateImpact: authz.Readonly}}

	auto := ComputeTier(TierInput{Handles: readonly, MaxAutoApproveHandles: 8})
	require.True(t, auto.AutoApproves(),
		"precondition: an all-readonly phase inside the budget is the tier-0 case")

	withSlot := ComputeTier(TierInput{
		Handles: readonly, MaxAutoApproveHandles: 8, UngrantedSlots: 1,
	})
	assert.False(t, withSlot.AutoApproves(),
		"approving this writes a grant on somebody's resource; a human decides")
}

// A slot the session ALREADY holds a grant for is not new authority — the
// approval that created it already happened. Re-pricing the phase for it would
// tax a plan for reach it demonstrably has, which is the fatigue the tier
// gradient exists to avoid.
func TestComputeTier_anAlreadyGrantedSlotDoesNotRaiseTheTier(t *testing.T) {
	readonly := []HandleImpact{{Handle: handle(t, "read", "doc"), StateImpact: authz.Readonly}}

	got := ComputeTier(TierInput{
		Handles: readonly, MaxAutoApproveHandles: 8, UngrantedSlots: 0,
	})
	assert.True(t, got.AutoApproves())
}
