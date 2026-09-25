// pkg/channels/channelsd/pipeline/start_approval_interaction_test.go
//
// The decision half of the session-START gate: the DecidePlatformAdmin
// standing check in the generic pipe, and decideStartApproval — the bound
// handler that unparks (approve) or fails (deny) a guest-started session.
//
// decideStartApproval tests call the handler directly, like decidePermission's
// tests: the generic pipe already ran the category's standing check before
// invoking it.
package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// startApprovalDecision builds the Decision a resolved start_approval click
// hands the bound handler.
func startApprovalDecision(sess *spiceboxv1alpha1.AgentSession, actionID, requestRef string) channelinteractions.Decision {
	return channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Payload: channelevents.InteractionDecisionPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
			Category:        categories.StartApproval,
			RequestRef:      requestRef,
			ActionID:        actionID,
			Decider:         channelevents.ExternalIdentity{Kind: "fake", ExternalID: "U_ADMIN", Email: "admin@example.com"},
		},
	}
}

// TestDecideStartApproval_ApproveGrantsAndUnparks: Approve writes the
// standing the parked create withheld — started_by for the guest and the
// class's interact-policy participant — removes the parking marker so the
// operator unparks, and clears the pending entry + condition.
func TestDecideStartApproval_ApproveGrantsAndUnparks(t *testing.T) {
	guest := guestIdentity("U_GUEST")
	sess := parkedStartSession(t, "s-parked", guest, true)
	ac := agentClassForTest(t, "fake_channel:C1#member")
	p, az, _, _, cli := newPipeline(t, sess, ac)

	out, err := p.decideStartApproval(context.Background(), startApprovalDecision(sess, "approve", "startappr-park-1"))
	require.NoError(t, err, "decideStartApproval approve")
	assert.Equal(t, channelevents.OutcomeApproved, out.Result, "outcome")

	require.Equal(t, 1, az.startedByCalls, "started_by written on approve")
	assert.Equal(t, canonicalID(guest), az.lastStartedBy, "started_by subject = the guest, from the pending entry")
	assert.Equal(t, 1, az.participantCalls, "interact-policy participant written on approve")
	assert.Equal(t, "fake_channel:C1#member", az.lastParticipantSubject, "participant = the class's effective interact permission")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "s-parked"}, &got))
	assert.False(t, spiceboxv1alpha1.StartApprovalPending(&got), "parking marker removed — the operator unparks off this")
	assert.Empty(t, got.Status.PendingRequesters, "pending entry cleared")
	cond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStartApprovalPending)
	require.NotNil(t, cond, "condition recomputed")
	assert.Equal(t, "False", string(cond.Status), "StartApprovalPending False after the decision")
	assert.Nil(t, got.Status.StartFailure, "approve must not fail the session")
}

// TestDecideStartApproval_DenyBlocksAndFails: Deny writes the denied tuple
// (so the guest's next message in this thread is silently dropped by the
// blocklist gate), sets the StartFailure signal the operator drives to
// Failed/StartDenied, and clears the pending entry. No standing is written.
func TestDecideStartApproval_DenyBlocksAndFails(t *testing.T) {
	guest := guestIdentity("U_GUEST")
	sess := parkedStartSession(t, "s-parked", guest, true)
	p, az, _, _, cli := newPipeline(t, sess)

	out, err := p.decideStartApproval(context.Background(), startApprovalDecision(sess, "deny", "startappr-park-1"))
	require.NoError(t, err, "decideStartApproval deny")
	assert.Equal(t, channelevents.OutcomeDenied, out.Result, "outcome")

	assert.Equal(t, 0, az.startedByCalls, "no started_by on deny")
	assert.Equal(t, 0, az.participantCalls, "no participant on deny")
	require.Equal(t, 1, az.deniedTouchCalls, "denied tuple written on deny")
	assert.Equal(t, canonicalID(guest).String(), az.lastDeniedUser, "denied subject = the guest")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "s-parked"}, &got))
	assert.False(t, spiceboxv1alpha1.StartApprovalPending(&got), "marker removed: the request is decided")
	assert.Empty(t, got.Status.PendingRequesters, "pending entry cleared")
	require.NotNil(t, got.Status.StartFailure, "StartFailure signal set — the operator drives Failed off it")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionStartDenied, got.Status.StartFailure.Reason)
}

// TestDecideStartApproval_StaleClickTouchesNothing: a requestRef with no
// matching pending entry (already decided, or expired) renders the outcome
// for the clicker's surface but writes nothing.
func TestDecideStartApproval_StaleClickTouchesNothing(t *testing.T) {
	guest := guestIdentity("U_GUEST")
	sess := parkedStartSession(t, "s-parked", guest, false)
	p, az, _, _, _ := newPipeline(t, sess)

	out, err := p.decideStartApproval(context.Background(), startApprovalDecision(sess, "approve", "startappr-gone-9"))
	require.NoError(t, err, "stale decideStartApproval")
	assert.Equal(t, channelevents.OutcomeApproved, out.Result, "outcome still rendered for the clicker")
	assert.Equal(t, 0, az.startedByCalls, "no writes on a stale click")
	assert.Equal(t, 0, az.deniedTouchCalls, "no writes on a stale click")
}

// TestRequestCategoryWitness_PendingRequesterCarriesCategory: a pending entry
// raised as start_approval must witness AS start_approval — if the witness
// answered permission_request (the historical default), a click claiming the
// owner-policy category would resolve an admin-policy request under the
// weaker check.
func TestRequestCategoryWitness_PendingRequesterCarriesCategory(t *testing.T) {
	guest := guestIdentity("U_GUEST")
	sess := parkedStartSession(t, "s-parked", guest, true)
	p, _, _, _, _ := newPipeline(t, sess)

	got, ok := p.requestCategoryWitness(context.Background(), "default", "s-parked", "startappr-park-1", nil, nil)
	require.True(t, ok, "the pending entry must witness")
	assert.Equal(t, categories.StartApproval, got, "witnessed category = the category the entry was raised under")
}

// TestHandleInteractionDecision_PlatformAdminPolicy pins the pipe's
// DecidePlatformAdmin standing check: platform#start_session decides, asked
// of the platform singleton — an ordinary participant or session owner has no
// say over a start_approval card.
func TestHandleInteractionDecision_PlatformAdminPolicy(t *testing.T) {
	cases := []struct {
		name        string
		isAdmin     bool
		wantHandled int
	}{
		{name: "platform admin: handler runs", isAdmin: true, wantHandled: 1},
		{name: "non-admin: rejected, handler never runs", isAdmin: false, wantHandled: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cat := registerInteractionCategory(t, channelinteractions.DecidePlatformAdmin)
			calls := 0
			channelinteractions.Bind(cat.Name, trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeApproved, OutcomeText: "Approved"}, nil))

			sess := existingSession(t, "s1", "U_OWNER", spiceboxv1alpha1.AgentSessionPhaseRunning)
			p, az, _, _, _ := newPipeline(t, sess)
			var asked [][2]string
			az.checkOwnerFn = func(resType, resID, _ string) (bool, error) {
				asked = append(asked, [2]string{resType, resID})
				return tc.isAdmin, nil
			}

			env := mustBuildInteractionDecision(t, client.ObjectKey{Namespace: "default", Name: "s1"},
				cat.Name, "req-1", "approve",
				channelevents.ExternalIdentity{Kind: "fake", ExternalID: "U_CLICKER", Email: "clicker@example.com"})
			require.NoError(t, p.HandleInteractionDecision(context.Background(), env))

			assert.Equal(t, tc.wantHandled, calls, "handler invocations")
			require.NotEmpty(t, asked, "standing must be checked against SpiceDB")
			assert.Equal(t, [2]string{"platform", "platform"}, asked[0],
				"the check is platform:platform#start_session, not a session or class permission")
		})
	}
}
