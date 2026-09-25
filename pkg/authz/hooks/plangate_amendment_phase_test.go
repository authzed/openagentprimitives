package hooks

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	plangateaudit "github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// unapprovedWritePhaseGate builds a gate whose ACTIVE phase is priced above
// tier 0, so freeze writes it no automatic clearance and it starts
// un-approved. Its ceiling holds a write handle and NOT the send handle, so a
// call on send is out of ceiling.
func unapprovedWritePhaseGate(t *testing.T, rec *fakeRecorder) *PlanGate {
	t.Helper()

	write := permDesc(t, "write", "tracker_issue", "update_issue", authz.Readwrite)
	send := permDesc(t, "send", "email", "send_email", authz.External)
	surface := []permsurface.Descriptor{write, send}

	plan, probs := plangate.FreezeFrom([]plangate.AuthoredPhase{
		{ID: "write", Label: "Write", Why: "edit the ticket",
			Permissions: []plangate.AuthoredPermission{{Handle: write.Handle.String(), Why: "to write"}}},
	}, surface, nil)
	require.Empty(t, probs)

	// CurrentPlan, not just Plan: the phase-approval gate engages only for a
	// DECLARED plan, and the escape being pinned here is one an agent reaches
	// by declaring a tier-1 phase with update_plan.
	return NewPlanGate(PlanGateDeps{
		Mode:        "enforcing",
		Plan:        plangate.SessionPlan(surface),
		CurrentPlan: func() (plangate.Plan, bool) { return plan, true },
		Surface:     surface,
		Resolve:     surfaceResolver(t, surface),
		Records:     func() []plangateaudit.Content { return nil },
		Recorder:    rec,
		Logger:      &fakeLogger{},
	})
}

// A justified OUT-of-ceiling call on an un-approved phase must raise the
// AMENDMENT, and specifically must not be turned into a phase-approval ask.
//
// Turning it into one looks like the obvious fix — phaseNeedsApproval runs only
// on the in-ceiling arm, so this path never challenges the phase — and it is a
// BYPASS. requestPhaseApproval returns Allow-with-ask, which is right on the
// in-ceiling arm where the handle is already inside the ceiling; the executor
// falls through with that verdict on a yes. Asking for the phase here would
// therefore let the OUT-of-ceiling call run on a yes, which is strictly worse
// than the amendment it replaced.
//
// What closes the escape is in the fold instead: an approved amendment records
// only the handle it named and no longer falls through to the whole-phase
// grant (TestFold_ApprovingAnAmendmentDoesNotApproveTheWholePhase). The phase
// stays un-approved, so the next in-ceiling call raises the phase card for
// real, and the human who approved the amendment got exactly what the card
// said: one handle.
func TestAmendment_AnUnapprovedPhaseStillRaisesTheAmendmentNotAPhaseAsk(t *testing.T) {
	rec := &fakeRecorder{}
	h := unapprovedWritePhaseGate(t, rec)

	d := evalToolWithReason(t, h, "send_email",
		"the ticket says to notify the reporter, so I need to send mail")

	require.NotNil(t, d.Approval, "a justified retry must reach a human")
	assert.Equal(t, "plan_amendment", d.Approval.Kind,
		"an out-of-ceiling call must be put to the human as the ONE addition it is; "+
			"a phase ask here would be answered Allow and let the out-of-ceiling call run")
}

// The legitimate amendment on an APPROVED phase is unchanged.
func TestAmendment_AnApprovedPhaseStillRaisesTheAmendment(t *testing.T) {
	rec := &fakeRecorder{}
	h := enforcingGate(t, rec, nil) // phase 0 is readonly, so tier 0 clears it

	first := evalTool(t, h, "update_issue")
	require.Equal(t, pipeline.Deny, first.Verdict)

	second := evalToolWithReason(t, h, "update_issue",
		"the issue body says the fix belongs in this same ticket, so I need to edit it")

	require.NotNil(t, second.Approval)
	assert.Equal(t, "plan_amendment", second.Approval.Kind,
		"widening a phase the human already cleared is exactly what the amendment is for")
}
