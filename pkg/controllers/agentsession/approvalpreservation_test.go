//go:build integration

// pkg/controllers/agentsession/approvalpreservation_test.go
//
// Integration test for the approval-condition-preservation bug: each operator
// status patch (restart.go / archive.go) previously used a whole-status-
// conditions-array MergeFrom that would wipe a concurrently-written channelsd
// approval condition. The canonical race is:
//
//  1. Operator reads sess (no approval condition yet).
//  2. channelsd writes PermissionRequestPending=True (concurrent write).
//  3. Operator patches with MergeFrom(stale_sess) → patch document contains
//     only the operator's own condition → approval condition wiped.
//
// The fix is to route operator status patches through agentstatus.WriteOwned,
// which (a) omits conditions from the scalar merge-patch and (b) reloads fresh
// before updating the operator-owned conditions, so concurrently-written
// approval conditions are always preserved.
package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// TestSupersedeParent_PreservesApprovalConditionUnderConcurrentChannelsdWrite
// demonstrates the whole-array MergeFrom race in SupersedeParent (restart.go).
// The operator holds a stale snapshot (no approval condition); channelsd writes
// PermissionRequestPending=True concurrently; SupersedeParent patches with the
// stale base → wipes the approval condition. RED before fix.
func TestSupersedeParent_PreservesApprovalConditionUnderConcurrentChannelsdWrite(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "supersede-preserves", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "c"},
	}
	require.NoError(t, env.Client.Create(ctx, parent))

	// Step 1: Operator reads the session BEFORE channelsd writes anything.
	// operatorSnapshot represents the stale in-memory copy the operator holds
	// throughout a reconcile pass (no approval conditions yet).
	var operatorSnapshot spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(parent), &operatorSnapshot))

	// Step 2: channelsd writes PermissionRequestPending=True to the live server
	// AFTER the operator's read — simulating the concurrent-write race.
	// operatorSnapshot is deliberately NOT updated; it remains stale.
	withApproval := operatorSnapshot.DeepCopy()
	apimeta.SetStatusCondition(&withApproval.Status.Conditions, metav1.Condition{
		Type:               spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending,
		Status:             metav1.ConditionTrue,
		Reason:             "PendingApproval",
		LastTransitionTime: metav1.Now(),
	})
	require.NoError(t, env.Client.Status().Patch(ctx, withApproval, client.MergeFrom(&operatorSnapshot)),
		"seed approval condition (simulated concurrent channelsd write)")

	// Step 3: Operator calls SupersedeParent with the STALE snapshot.
	// The stale base has no approval condition; the whole-array MergeFrom patch
	// therefore sends conditions=[SupersededByRestart=True] and overwrites the
	// server's [PermissionRequestPending=True].
	require.NoError(t, agentsession.SupersedeParent(ctx, env.Client, &operatorSnapshot, "child-name"))

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(parent), &got))

	// The approval condition must survive the operator's status write — RED before fix.
	approvalCond := apimeta.FindStatusCondition(got.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending)
	assert.NotNil(t, approvalCond,
		"PermissionRequestPending=True must survive the operator SupersedeParent patch "+
			"(concurrent channelsd write race — clobbered by whole-array MergeFrom)")

	// The operator condition must also be present (proves the function ran).
	opCond := apimeta.FindStatusCondition(got.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionSupersededByRestart)
	assert.NotNil(t, opCond, "SupersededByRestart must be set after SupersedeParent")
}
