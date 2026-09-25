//go:build integration

// pkg/controllers/agentsession/statusapply_test.go
//
// Envtest integration test for applyStatus: verifies that the operator's SSA
// status apply (field-manager "agentsession") does NOT clobber
// channelsd-owned fields (pendingToolGrants) that the operator never claimed.
// A real API server is required for SSA field-manager tracking — hence envtest.
package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// meta_SetCondition is a test helper that sets a single condition on sess.Status
// via meta.SetStatusCondition. Defined here (not in export_test.go) because it
// accesses no unexported agentsession symbols.
func meta_SetCondition(sess *spiceboxv1alpha1.AgentSession, condType string, status metav1.ConditionStatus) {
	apimeta.SetStatusCondition(&sess.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             "Testing",
	})
}

// TestApplyStatus_DoesNotClobberApprovalFields verifies that the operator's SSA
// status apply (field-manager "agentsession") does NOT clobber channelsd-owned
// pending approval fields (pendingToolGrants) that the operator never claimed.
func TestApplyStatus_DoesNotClobberApprovalFields(t *testing.T) {
	ctx := context.Background()
	env := testenv.Shared(t)

	as := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "as-ssa", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "c"},
	}
	require.NoError(t, env.Client.Create(ctx, as))

	// Simulate channelsd writing a pending interaction (its approval surface)
	// via a regular merge patch. SSA field-manager ownership is what prevents
	// the operator's subsequent SSA apply from clobbering this field; the field
	// just needs to exist on the server before the operator applies.
	cp := as.DeepCopy()
	cp.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{{
		RequestID:       "g1",
		Category:        "tool_approval",
		ApproverSubject: "agentsession:default/as-ssa#approve",
		RequestRef:      "g1",
		RequestedAt:     metav1.Now(),
		Summary:         "test_tool",
	}}
	require.NoError(t, env.Client.Status().Patch(ctx, cp, client.MergeFrom(as)))

	// Re-fetch to get the server's current state (including the pending
	// interaction) so we can set operator-owned fields on the live object before
	// applying.
	var live spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(as), &live))

	// Set an operator-owned lifecycle condition and apply via SSA.
	meta_SetCondition(&live, spiceboxv1alpha1.AgentSessionConditionRunnerReady, metav1.ConditionTrue)
	r := newReconciler(t, env)
	require.NoError(t, r.ApplyStatusForTest(ctx, &live))

	// The SSA apply must have landed the operator's condition AND left the
	// channelsd-owned pending interaction untouched — SSA only manages fields
	// claimed by the "agentsession" field-manager, which never includes
	// pendingInteractions.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(as), &got))
	assert.Len(t, got.Status.PendingInteractions, 1,
		"operator SSA must NOT clobber channelsd's pending interaction")
	assert.NotNil(t, apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady),
		"operator-owned RunnerReady condition must be present after SSA apply")
}

// TestReconcile_StartFailureTransitionsFailed verifies that the operator's
// Reconcile detects status.startFailure (set by channelsd on authz-write
// failure) and transitions the session to phase=Failed with a Failed=True
// condition and the failure reason carried forward. This is the L2 ownership
// split: channelsd signals, operator drives the Failed state.
func TestReconcile_StartFailureTransitionsFailed(t *testing.T) {
	ctx := context.Background()
	env := testenv.Shared(t)

	ac := validClass("ac-sf")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sf1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "ac-sf",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "do the task"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Simulate channelsd writing startFailure via merge-patch (the L2 signal
	// path): phase and failureReason are intentionally left unset, matching
	// what the updated pipeline code writes.
	cp := sess.DeepCopy()
	cp.Status.StartFailure = &spiceboxv1alpha1.AgentSessionStartFailure{
		Reason:  spiceboxv1alpha1.ReasonAgentSessionAuthzWriteFail,
		Message: "spicedb write failed: connection refused",
	}
	require.NoError(t, env.Client.Status().Patch(ctx, cp, client.MergeFrom(sess)),
		"simulate channelsd startFailure patch")

	r := newReconciler(t, env)
	// Two reconciles: first installs finalizer, second detects startFailure.
	for i := 0; i < 2; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get AgentSession")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"operator must transition phase to Failed on startFailure signal")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionAuthzWriteFail, got.Status.FailureReason,
		"operator must carry startFailure.Reason into failureReason")
	assert.NotNil(t, got.Status.FinishedAt,
		"operator must stamp finishedAt on the Failed transition")
	failedCond := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	require.NotNil(t, failedCond, "Failed condition must be set")
	assert.Equal(t, metav1.ConditionTrue, failedCond.Status, "Failed condition status")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionAuthzWriteFail, failedCond.Reason,
		"Failed condition reason must match startFailure.Reason")
}
