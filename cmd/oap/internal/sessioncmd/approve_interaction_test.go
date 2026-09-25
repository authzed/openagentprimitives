// content_inspection is parked on the generic Status.PendingInteractions
// list (Task 8) — the legacy Status.PendingContentInspectionApprovals typed
// list it used to dual-read from during the migration was deleted outright
// in Task 10. `oap session approve` collects it from PendingInteractions
// (collectPendings) and drives its decision through the generic
// interaction_decision/interaction_applied envelopes (dispatchFor) — see
// TestBuildDecisionPayload_PerKind / TestDecodeApplied_PerKind in
// session_approve_test.go for the dispatch-side coverage. This file covers
// the collection side: a PendingInteractions entry must resolve to the same
// approvalKindContentInspection the CLI already knows how to
// disambiguate/display/dispatch.
package sessioncmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// TestCollectPendings_GenericInteractionList verifies collectPendings folds a
// content_inspection entry parked on the generic PendingInteractions list
// (Task 8's producer-side migration) into a pendingEntry tagged
// approvalKindContentInspection — the same tag the CLI already knows how to
// disambiguate/display/dispatch. See TestCollectPendings in
// session_approve_test.go for coverage of this alongside tool_call/leakage_share.
func TestCollectPendings_GenericInteractionList(t *testing.T) {
	t.Run("content_inspection on PendingInteractions → pendingEntry{Kind: approvalKindContentInspection}", func(t *testing.T) {
		sess := &spiceboxv1alpha1.AgentSession{}
		sess.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{
			{
				RequestID:       "ci-generic-1",
				Category:        categories.ContentInspection,
				ApproverSubject: "agentsession:ns/sess#approve",
				RequestRef:      "ci-generic-1",
				RequestedAt:     metav1.Now(),
				// AgentDisplayName deliberately empty — C1 does not stamp it
				// (Task 8 dropped the visible-Field lookup; population deferred to
				// C2). Consumers that display it must tolerate empty.
				Summary: "suspected prompt injection flagged in tool output",
			},
		}
		got := collectPendings(sess)
		require.Len(t, got, 1)
		assert.Equal(t, pendingEntry{
			RequestID:       "ci-generic-1",
			ApproverSubject: "agentsession:ns/sess#approve",
			Kind:            approvalKindContentInspection,
		}, got[0])
	})

	t.Run("non-content_inspection category on the generic list is ignored (C1: only content_inspection lives there)", func(t *testing.T) {
		sess := &spiceboxv1alpha1.AgentSession{}
		sess.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{
			{RequestID: "other-1", Category: "some_future_category"},
		}
		got := collectPendings(sess)
		assert.Empty(t, got)
	})

	t.Run("resolvePending auto-picks a single generic-list content_inspection entry", func(t *testing.T) {
		sess := &spiceboxv1alpha1.AgentSession{}
		sess.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{
			{RequestID: "ci-generic-1", Category: categories.ContentInspection, ApproverSubject: "agentsession:ns/sess#approve"},
		}
		pendings := collectPendings(sess)
		matched, err := resolvePending(pendings, "", "ns", "sess")
		require.NoError(t, err)
		assert.Equal(t, approvalKindContentInspection, matched.Kind)
		assert.Equal(t, "ci-generic-1", matched.RequestID)
	})
}

// TestDispatchFor_ContentInspection_PublishesGenericInteractionDecision pins
// the C1 behavior change: content_inspection's dispatch table entry targets
// the generic interaction_decision/interaction_applied kinds, NOT the legacy
// Kind*ContentInspectionApproval* pair (deleted outright in Task 10).
func TestDispatchFor_ContentInspection_PublishesGenericInteractionDecision(t *testing.T) {
	dispatch, ok := dispatchFor(approvalKindContentInspection)
	require.True(t, ok)
	assert.Equal(t, channelevents.KindInteractionDecision, dispatch.decisionKind)
	assert.Equal(t, channelevents.KindInteractionApplied, dispatch.appliedKind)
}
