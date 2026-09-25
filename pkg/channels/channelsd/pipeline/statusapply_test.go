//go:build integration

package pipeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// TestApplyApprovalStatus_DoesNotClobberLifecycle verifies that channelsd's
// approval-status write via WriteOwned does NOT delete operator-owned lifecycle
// conditions. The operator owns RunnerReady; channelsd owns the *ApprovalPending
// conditions and pending queues. After channelsd writes its surface, the
// operator's RunnerReady must survive.
func TestApplyApprovalStatus_DoesNotClobberLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	env := testenv.Shared(t)
	as := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "as-appr", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "c"},
	}
	require.NoError(t, env.Client.Create(ctx, as))

	// Stand-in for the operator: write RunnerReady=True via Status().Update().
	meta.SetStatusCondition(&as.Status.Conditions, metav1.Condition{
		Type:               spiceboxv1alpha1.AgentSessionConditionRunnerReady,
		Status:             metav1.ConditionTrue,
		Reason:             "R",
		LastTransitionTime: metav1.Now(),
	})
	require.NoError(t, env.Client.Status().Update(ctx, as))

	// Read a fresh copy as `original` — the snapshot WriteOwned diffs against.
	var original spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(as), &original))

	// Build the mutated copy: add a pending interaction (a tool_approval park —
	// the channelsd-owned pending-approval surface) and the corresponding
	// ToolApprovalPending condition. All required PendingInteraction fields are
	// set so the apiserver does not prune unknown fields.
	mutated := original.DeepCopy()
	mutated.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{{
		RequestID:       "g1",
		Category:        "tool_approval",
		ApproverSubject: "agentsession:default/as-appr#approve",
		RequestRef:      "g1",
		RequestedAt:     metav1.Now(),
		Summary:         "test_tool",
	}}
	meta.SetStatusCondition(&mutated.Status.Conditions, metav1.Condition{
		Type:               spiceboxv1alpha1.AgentSessionConditionToolApprovalPending,
		Status:             metav1.ConditionTrue,
		Reason:             "P",
		LastTransitionTime: metav1.Now(),
	})
	require.NoError(t, pipeline.ApplyApprovalStatusForTest(ctx, env.Client, mutated, &original))

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(as), &got))
	assert.NotNil(t, meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady),
		"channelsd write must NOT clobber the operator's RunnerReady")
	assert.NotNil(t, meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionToolApprovalPending),
		"ToolApprovalPending condition must be present after channelsd write")
	assert.Len(t, got.Status.PendingInteractions, 1, "pending interaction must be persisted")
}
