package hooks

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// planApprovedRecs is what the runner writes when a plan freezes: one record
// per phase, carrying that phase's full authority so a later reader can rebuild
// what was approved.
func planApprovedRecs(p plangate.Plan) []plangateaudit.Content {
	var out []plangateaudit.Content
	for i, ph := range p.Phases {
		idx := int32(i)
		ceiling := make([]string, 0, len(ph.Permissions))
		for _, h := range ph.Permissions {
			ceiling = append(ceiling, h.String())
		}
		out = append(out, plangateaudit.Content{
			Event:      plangateaudit.EventPlanApproved,
			PlanDigest: p.Digest(),
			PhaseIndex: &idx,
			Ceiling:    ceiling,
			MaxCount:   ph.Max.Count,
		})
	}
	return out
}

// widerPlan is a PRIOR plan holding strictly more than approvalGate's phase 0:
// the same read reach plus a write handle. A human cleared it. The gate's own
// plan is therefore a NARROWING of something already approved.
func widerPlan(t *testing.T) plangate.Plan {
	t.Helper()
	surface := demoSurface(t)
	p, probs := plangate.FreezeFrom([]plangate.AuthoredPhase{
		{ID: "recon", Label: "Recon", Why: "read and write", Permissions: []plangate.AuthoredPermission{
			{Handle: "perm:read:tracker_issue", Why: "to read"},
			{Handle: "perm:write:tracker_issue", Why: "to write"},
		}},
	}, surface, nil)
	require.Empty(t, probs)
	return p
}

// The wiring, which a plangate-package test cannot see: the gate must actually
// CONSULT carry-over. An agent that re-plans to ask for LESS than a human
// already cleared must not be sent back to that human.
func TestPhaseApproval_narrowedReplanDoesNotReAsk(t *testing.T) {
	prior := widerPlan(t)
	idx := int32(0)
	records := append(planApprovedRecs(prior), plangateaudit.Content{
		Event: plangateaudit.EventPhaseApproved, PlanDigest: prior.Digest(), PhaseIndex: &idx,
	})

	rec := &fakeRecorder{}
	h, _ := approvalGate(t, rec, records) // its own plan holds read only

	d := evalTool(t, h, "read_issue")

	assert.Nil(t, d.Approval,
		"the approved plan already held this reach and more; asking again is the "+
			"fatigue this slice removes")
	assert.NotEqual(t, pipeline.Deny, d.Verdict)
}

// The other half, and the one that matters: a prior approval must not become a
// skeleton key. The gate's plan holding reach the approved plan did NOT hold
// still reaches a human.
func TestPhaseApproval_priorApprovalDoesNotCoverNewReach(t *testing.T) {
	// The prior plan is NARROWER than the gate's: it never held read.
	surface := demoSurface(t)
	narrow, probs := plangate.FreezeFrom([]plangate.AuthoredPhase{
		{ID: "recon", Label: "Recon", Why: "write only", Permissions: []plangate.AuthoredPermission{
			{Handle: "perm:write:tracker_issue", Why: "to write"},
		}},
	}, surface, nil)
	require.Empty(t, probs)

	idx := int32(0)
	records := append(planApprovedRecs(narrow), plangateaudit.Content{
		Event: plangateaudit.EventPhaseApproved, PlanDigest: narrow.Digest(), PhaseIndex: &idx,
	})

	rec := &fakeRecorder{}
	h, _ := approvalGate(t, rec, records)

	d := evalTool(t, h, "read_issue")

	require.NotNil(t, d.Approval,
		"read reach was never approved; an unrelated earlier yes cannot supply it")
	assert.Equal(t, "plan_phase", d.Approval.Kind)
}

// "One card, not N" — the slice's headline claim.
//
// The gate's phase holds read reach; the plan a human approved held neither
// that nor the write handle. Both additions must arrive in ONE card, because
// the alternative is what the batch fix exists to remove: denied at dispatch on
// the first new handle, asked about it, denied again on the second.
func TestPhaseApproval_aWideningShowsEveryAdditionOnOneCard(t *testing.T) {
	surface := demoSurface(t)
	// A prior plan a human cleared, holding NEITHER handle the gate's plan wants.
	prior, probs := plangate.FreezeFrom([]plangate.AuthoredPhase{
		{ID: "recon", Label: "Recon", Why: "nothing yet", Permissions: nil},
	}, surface, nil)
	require.Empty(t, probs)

	idx := int32(0)
	records := append(planApprovedRecs(prior), plangateaudit.Content{
		Event: plangateaudit.EventPhaseApproved, PlanDigest: prior.Digest(), PhaseIndex: &idx,
	})

	rec := &fakeRecorder{}
	h, plan := approvalGate(t, rec, records)
	require.NotEmpty(t, plan.Phases[0].Permissions)

	d := evalTool(t, h, "read_issue")

	require.NotNil(t, d.Approval, "the phase widens what was approved, so it must ask")
	require.NotEmpty(t, rec.recs, "the card must be recorded")

	var card string
	for _, r := range rec.recs {
		if r.CardJSON != "" {
			card = r.CardJSON
		}
	}
	require.NotEmpty(t, card, "a card must have been built")
	assert.Contains(t, card, "Add to the approved plan",
		"a widening states the DELTA, not the whole ceiling an approver already read")
	for _, h := range plan.Phases[0].Permissions {
		assert.Contains(t, card, describedOnCard(t, h),
			"every addition must appear on the one card; a handle left off is one "+
				"the agent gets denied on later, which is the interruption this removes")
	}
}

// describedOnCard is how a handle READS on a card — the resolved description,
// not the wire form. Asserting on it rather than on the raw handle is the
// stronger claim: it pins that the right handle is present AND that an approver
// can tell what it grants. No title is declared in these fixtures, so the card
// carries DescribeWithTitle's detokenized fallback, not Describe()'s
// tool-anchored form.
func describedOnCard(t *testing.T, h permsurface.Handle) string {
	t.Helper()
	for _, d := range demoSurface(t) {
		if d.Handle == h {
			return d.DescribeWithTitle("")
		}
	}
	require.FailNow(t, "handle is not on the demo surface", h.String())
	return ""
}

// A FIRST approval has nothing to diff against, and must state the full
// ceiling — all of it is new. Without this the delta path would render an empty
// "Add to the approved plan:" and tell the approver nothing.
func TestPhaseApproval_firstApprovalStatesTheWholeCeiling(t *testing.T) {
	rec := &fakeRecorder{}
	h, plan := approvalGate(t, rec, nil) // no prior approvals at all

	d := evalTool(t, h, "read_issue")
	require.NotNil(t, d.Approval)

	var card string
	for _, r := range rec.recs {
		if r.CardJSON != "" {
			card = r.CardJSON
		}
	}
	require.NotEmpty(t, card)
	assert.NotContains(t, card, "Add to the approved plan",
		"nothing was approved before, so there is no delta to state")
	assert.Contains(t, card, describedOnCard(t, plan.Phases[0].Permissions[0]))
}

// The wiring: a re-plan that adds a SLOT widens, so a human is asked — and the
// card they are asked with must name it. The gate computes the delta; if it
// passes only the handle additions on, the approver is interrupted by a
// widening the card describes none of.
func TestPhaseApproval_aWideningThatAddsOnlyASlotStillDescribesIt(t *testing.T) {
	surface := demoSurface(t)
	// The approved plan held the same handle and NO slot; the gate's plan adds one.
	prior, probs := plangate.FreezeFrom([]plangate.AuthoredPhase{
		{ID: "recon", Label: "Recon", Why: "read", Permissions: []plangate.AuthoredPermission{
			{Handle: "perm:read:tracker_issue", Why: "to read"},
		}},
	}, surface, nil)
	require.Empty(t, probs)

	current, probs := plangate.FreezeFrom([]plangate.AuthoredPhase{
		{ID: "recon", Label: "Recon", Why: "read", Permissions: []plangate.AuthoredPermission{
			{Handle: "perm:read:tracker_issue", Why: "to read"},
		}, Slots: []plangate.AuthoredSlot{{Type: "crm_company", Why: "to reach it"}}},
	}, surface, []string{"crm_company"})
	require.Empty(t, probs)

	idx := int32(0)
	records := append(planApprovedRecs(prior), plangateaudit.Content{
		Event: plangateaudit.EventPhaseApproved, PlanDigest: prior.Digest(), PhaseIndex: &idx,
	})

	rec := &fakeRecorder{}
	h := NewPlanGate(PlanGateDeps{
		Mode:        "enforcing",
		Plan:        plangate.SessionPlan(surface),
		CurrentPlan: func() (plangate.Plan, bool) { return current, true },
		Surface:     surface,
		Resolve:     surfaceResolver(t, surface),
		Records:     func() []plangateaudit.Content { return records },
		Recorder:    rec,
		Logger:      &fakeLogger{},
	})

	d := evalTool(t, h, "read_issue")
	require.NotNil(t, d.Approval, "adding a slot widens, so it must reach a human")

	var card string
	for _, r := range rec.recs {
		if r.CardJSON != "" {
			card = r.CardJSON
		}
	}
	require.NotEmpty(t, card)
	assert.Contains(t, card, "Add to the approved plan",
		"a widening states the DELTA, whichever axis it widened on")
	assert.Contains(t, card, "crm_company",
		"the only thing that changed is the slot; a card omitting it explains nothing")
}
