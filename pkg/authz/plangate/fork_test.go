package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// A child session starts from ONE derived record, not from copies of the
// parent's chain. The parent's log stays intact and independently verifiable in
// its own scope; the child's chain begins at a single honest root.
func TestDeriveForFork_producesExactlyOneRecord(t *testing.T) {
	p := threePhasePlan(t)

	got, err := DeriveForFork(ForkInput{
		Mode:   ForkInherit,
		Parent: "ns/parent",
		Plan:   p,
		Records: []plangateaudit.Content{
			approveRec(p), selectRec(p, 1), denyRec(p, 2),
		},
	})
	require.NoError(t, err)

	assert.Equal(t, plangateaudit.EventPlanApproved, got.Event)
	assert.Contains(t, got.Provenance, "ns/parent",
		"the derived root must name where its authority came from")
}

// The resolved OUTCOME travels, not the steps that produced it. Ordering is
// then irrelevant in the child because there is nothing left to order.
func TestDeriveForFork_carriesTheResolvedState(t *testing.T) {
	p := threePhasePlan(t)

	got, err := DeriveForFork(ForkInput{
		Mode:   ForkInherit,
		Parent: "ns/parent",
		Plan:   p,
		Records: []plangateaudit.Content{
			approveRec(p), selectRec(p, 1),
		},
	})
	require.NoError(t, err)

	assert.Equal(t, p.Digest(), got.PlanDigest)
	require.NotNil(t, got.PhaseIndex)
	assert.Equal(t, int32(1), *got.PhaseIndex, "the child resumes where the parent left off")
}

// A denial the parent's human made must survive the fork, or forking becomes a
// laundering step: deny, fork, and the child no longer knows.
func TestDeriveForFork_carriesDenialsForward(t *testing.T) {
	p := threePhasePlan(t)

	got, err := DeriveForFork(ForkInput{
		Mode:    ForkInherit,
		Parent:  "ns/parent",
		Plan:    p,
		Records: []plangateaudit.Content{approveRec(p), denyRec(p, 1)},
	})
	require.NoError(t, err)

	// Fold the child's single record and confirm the denial is still in force.
	child, err := Fold(p, []plangateaudit.Content{got})
	require.NoError(t, err)
	assert.True(t, child.DeniedCeilingIntersects(p.Phases[1].Permissions),
		"a human's no must survive a fork")
}

// takeover is a DIFFERENT user continuing a terminal session, and they become
// the child's owner. Inheriting would hand them the previous owner's
// human-approved ceilings on resources they never had standing on — and the
// agentsession#fork gate does not even run for this mode, so nothing else
// stops it.
func TestDeriveForFork_takeoverInheritsNothing(t *testing.T) {
	p := threePhasePlan(t)

	got, err := DeriveForFork(ForkInput{
		Mode:    ForkTakeover,
		Parent:  "ns/parent",
		Plan:    p,
		Records: []plangateaudit.Content{approveRec(p), selectRec(p, 1)},
	})
	require.NoError(t, err)

	assert.Empty(t, got.Ceiling, "a taken-over session starts with no inherited reach")
	assert.Empty(t, got.PlanDigest, "and no inherited plan")

	child, err := Fold(Plan{}, []plangateaudit.Content{got})
	require.NoError(t, err)
	_, cerr := child.ActiveCeiling()
	assert.Error(t, cerr, "the child holds nothing until it plans and is approved afresh")
}

// The envelope is the pre-exposure baseline for the TASK, so an inheriting
// child keeps it: re-deriving one after the parent has read external text
// would launder post-exposure reach into a "pre-exposure" baseline.
func TestDeriveForFork_carriesTheEnvelopeForward(t *testing.T) {
	p := threePhasePlan(t)

	got, err := DeriveForFork(ForkInput{
		Mode:   ForkInherit,
		Parent: "ns/parent",
		Plan:   p,
		Records: []plangateaudit.Content{
			{Event: plangateaudit.EventEnvelope, Ceiling: []string{"perm:read:tracker_issue"}},
			approveRec(p),
		},
	})
	require.NoError(t, err)

	assert.Contains(t, got.Envelope, "perm:read:tracker_issue")
}

func TestDeriveForFork_takeoverDropsTheEnvelopeToo(t *testing.T) {
	p := threePhasePlan(t)

	got, err := DeriveForFork(ForkInput{
		Mode:   ForkTakeover,
		Parent: "ns/parent",
		Plan:   p,
		Records: []plangateaudit.Content{
			{Event: plangateaudit.EventEnvelope, Ceiling: []string{"perm:read:tracker_issue"}},
		},
	})
	require.NoError(t, err)

	assert.Empty(t, got.Envelope,
		"a different owner's task has its own baseline, not the previous owner's")
}

// A parent whose fold could not be trusted must not hand the child a
// confident-looking root. The child re-plans rather than inheriting a guess.
func TestDeriveForFork_doubtfulParentInheritsNothing(t *testing.T) {
	p := threePhasePlan(t)
	bad := int32(99)

	got, err := DeriveForFork(ForkInput{
		Mode:   ForkInherit,
		Parent: "ns/parent",
		Plan:   p,
		Records: []plangateaudit.Content{
			approveRec(p),
			{Event: plangateaudit.EventPhaseSelected, PlanDigest: p.Digest(), PhaseIndex: &bad},
		},
	})
	require.NoError(t, err)

	assert.Empty(t, got.PlanDigest,
		"an untrustworthy parent state must not become a trustworthy-looking child root")
}

func TestDeriveForFork_rejectsAnUnknownMode(t *testing.T) {
	_, err := DeriveForFork(ForkInput{Mode: "sideways", Parent: "ns/p", Plan: threePhasePlan(t)})
	assert.Error(t, err, "an unrecognized fork mode must fail closed, not guess at inheritance")
}

// The reconstruction invariant everything else depends on: a plan rebuilt from
// the log must digest IDENTICALLY to the one that was recorded. If it does
// not, a fold against the rebuilt plan discards every record as belonging to
// another plan — which is exactly how the child silently inherited nothing.
func TestPlanFromRecords_digestMatchesTheRecordedPlan(t *testing.T) {
	surface := surfaceWith(t, "perm:read:tracker_issue", "perm:write:tracker_issue")
	a := authored("recon", "Recon", "perm:read:tracker_issue")
	b := authored("write", "Write", "perm:write:tracker_issue")
	b.Max = &AuthoredMax{Count: 3, Why: "retries"}
	b.Requires = []AuthoredRequires{{Phase: "recon", Why: "read first"}}

	original, probs := FreezeFrom([]AuthoredPhase{a, b}, surface, nil)
	require.Empty(t, probs)

	// The records FreezeAndRecordPhases would write.
	var recs []plangateaudit.Content
	for i, ph := range original.Phases {
		idx := int32(i)
		rec := plangateaudit.Content{
			Event: plangateaudit.EventPlanApproved, PlanDigest: original.Digest(),
			PhaseIndex: &idx, MaxCount: ph.Max.Count,
		}
		for _, h := range ph.Permissions {
			rec.Ceiling = append(rec.Ceiling, h.String())
		}
		for _, r := range ph.Requires {
			rec.Requires = append(rec.Requires, r.Phase)
		}
		recs = append(recs, rec)
	}

	rebuilt, ok := PlanFromRecords(recs)
	require.True(t, ok)
	assert.Equal(t, original.Digest(), rebuilt.Digest(),
		"a plan rebuilt from the log must be the same plan by digest")
}

func TestPlanFromRecords_noApprovalYieldsNoPlan(t *testing.T) {
	_, ok := PlanFromRecords([]plangateaudit.Content{
		{Event: plangateaudit.EventGateAllowed, PlanDigest: "d"},
	})
	assert.False(t, ok)
}

// The same reconstruction invariant, over a plan whose slots NAME THEIR
// INSTANCE — and via the projection production actually writes, not a
// hand-built record.
//
// Both halves are the point. A slot id is authority: it enters the digest, so a
// rebuild that recovered only the TYPE produced a different plan, every fold
// discarded every record as another plan's, and a restart silently lost the
// whole gate state — approvals, denials, active phase. And the older test could
// not have caught it, because it builds the record itself: a field production
// forgets is a field the fixture forgets identically.
func TestPlanFromRecords_digestSurvivesASlotThatNamesItsInstance(t *testing.T) {
	surface := demoSurface(t)
	original, probs := FreezeFrom([]AuthoredPhase{
		{ID: "recon", Label: "Recon", Why: "read first",
			Permissions: []AuthoredPermission{{Handle: "perm:read:tracker_issue", Why: "to read"}},
			Slots:       []AuthoredSlot{{Type: "crm_company", ID: "4210", Why: "the company the user named"}}},
		{ID: "ship", Label: "Ship", Why: "then write",
			Permissions: []AuthoredPermission{{Handle: "perm:write:tracker_issue", Why: "to write"}},
			Slots:       []AuthoredSlot{{Type: "crm_company", ID: "4299", Why: "the other one"}}},
	}, surface, []string{"crm_company"})
	require.Empty(t, probs)

	var recs []plangateaudit.Content
	for i := range original.Phases {
		rec := PhaseAuthorityRecord(original, i, nil)
		rec.Event = plangateaudit.EventPlanApproved
		rec.PlanDigest = original.Digest()
		recs = append(recs, rec)
	}

	rebuilt, ok := PlanFromRecords(recs)
	require.True(t, ok)
	assert.Equal(t, original.Digest(), rebuilt.Digest(),
		"a plan naming its targets must rebuild as ITSELF, or a restart discards the log")

	for i := range original.Phases {
		assert.Equal(t, original.Phases[i].AuthorityKey(), rebuilt.Phases[i].AuthorityKey(),
			"and each phase must keep its authority key, which is what approval is recorded on")
	}
}
