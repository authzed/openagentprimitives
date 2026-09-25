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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// seedSessionWithEffectiveSettings creates a channel-attached AgentSession and
// stamps an operator-owned effectiveSettings snapshot (reportSessionCost=true)
// onto its status, the way the operator does when parking a passthrough session.
func seedSessionWithEffectiveSettings(ctx context.Context, t *testing.T, env *testenv.Env, name string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	as := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "coder-bot"},
	}
	require.NoError(t, env.Client.Create(ctx, as), "create session")

	as.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials
	as.Status.EffectiveSettings = &spiceboxv1alpha1.EffectiveSettings{
		Model: spiceboxv1alpha1.ModelConfig{Name: "claude-sonnet-5", Provider: "anthropic"},
		// Budget is required by the CRD (maxTurns/maxTokens >= 1, maxDuration set).
		Budget: spiceboxv1alpha1.BudgetConfig{
			MaxTurns:    360,
			MaxTokens:   40000000,
			MaxDuration: metav1.Duration{Duration: time.Hour},
		},
		ReportSessionCost: true,
	}
	require.NoError(t, env.Client.Status().Update(ctx, as), "stamp operator-owned effectiveSettings")
	return as
}

// getSessionUnstructured reads the AgentSession as an unstructured object — the
// faithful stand-in for a version-skewed writer, whose compiled Go types may not
// even contain a field the CRD now has.
func getSessionUnstructured(ctx context.Context, t *testing.T, env *testenv.Env, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(spiceboxv1alpha1.SchemeGroupVersion.WithKind("AgentSession"))
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, u), "get unstructured")
	return u
}

// appendUnstructuredCondition appends a fully-populated status condition to the
// unstructured object's status.conditions array (all fields the metav1.Condition
// schema marks required, so the apiserver does not reject on a malformed entry).
func appendUnstructuredCondition(t *testing.T, u *unstructured.Unstructured, condType, reason string) {
	t.Helper()
	conds, _, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	require.NoError(t, err)
	conds = append(conds, map[string]any{
		"type":               condType,
		"status":             "True",
		"reason":             reason,
		"message":            "",
		"lastTransitionTime": metav1.Now().Format(time.RFC3339),
	})
	require.NoError(t, unstructured.SetNestedSlice(u.Object, conds, "status", "conditions"))
}

// TestConditionWrite_VersionSkewedRequiredField is the integration-level
// reproduction of the credential-request ephemeral re-send loop, run against a
// real apiserver enforcing the real AgentSession CRD (which marks
// status.effectiveSettings.reportSessionCost required).
//
// It demonstrates, at the API layer, why the write shape decides whether the
// loop happens:
//
//   - A full status Update that drops the required reportSessionCost (what a
//     channelsd binary predating the field does on a Get->Update round-trip) is
//     rejected — so the dedup condition never persists, and the ephemeral
//     re-sends forever.
//   - A conditions-only, optimistic-locked merge patch — the shape WriteOwned
//     uses — succeeds even from that same field-blind writer, and the
//     server-side reportSessionCost is preserved.
//   - WriteOwned itself (via applyApprovalStatus) stamps the dedup condition
//     against the real CRD and preserves operator-owned effectiveSettings.
func TestConditionWrite_VersionSkewedRequiredField(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	env := testenv.Shared(t)

	const credCond = spiceboxv1alpha1.AgentSessionConditionCredentialRequestPublished
	const credReason = spiceboxv1alpha1.ReasonCredentialRequestPublished

	t.Run("full status Update dropping required reportSessionCost is rejected (root cause)", func(t *testing.T) {
		seedSessionWithEffectiveSettings(ctx, t, env, "skew-fullupdate")

		u := getSessionUnstructured(ctx, t, env, "skew-fullupdate")
		got, found, err := unstructured.NestedBool(u.Object, "status", "effectiveSettings", "reportSessionCost")
		require.NoError(t, err)
		require.True(t, found && got, "precondition: server holds reportSessionCost=true")

		// Simulate the old channelsd binary: its struct has no reportSessionCost,
		// so a Get->Update round-trip drops it; it then adds its dedup condition
		// and Updates the whole status.
		unstructured.RemoveNestedField(u.Object, "status", "effectiveSettings", "reportSessionCost")
		appendUnstructuredCondition(t, u, credCond, credReason)

		err = env.Client.Status().Update(ctx, u)
		require.Error(t, err, "a full status Update missing a required field must be rejected by the apiserver")
		assert.Contains(t, err.Error(), "reportSessionCost",
			"rejection must be the reportSessionCost required-field violation (the production error)")
	})

	t.Run("conditions-only optimistic patch survives a field-blind writer and preserves reportSessionCost (fix shape)", func(t *testing.T) {
		seedSessionWithEffectiveSettings(ctx, t, env, "skew-condpatch")

		// prior = the field-blind writer's view (reportSessionCost stripped),
		// carrying the current resourceVersion for the optimistic lock.
		prior := getSessionUnstructured(ctx, t, env, "skew-condpatch")
		unstructured.RemoveNestedField(prior.Object, "status", "effectiveSettings", "reportSessionCost")

		// mutated = prior + the dedup condition. Because prior and mutated differ
		// ONLY in status.conditions, the merge patch body carries only conditions
		// (and metadata.resourceVersion) — never effectiveSettings.
		mutated := prior.DeepCopy()
		appendUnstructuredCondition(t, mutated, credCond, credReason)

		require.NoError(t,
			env.Client.Status().Patch(ctx, mutated, client.MergeFromWithOptions(prior, client.MergeFromWithOptimisticLock{})),
			"a conditions-only merge patch must not trip reportSessionCost required-validation")

		// The dedup condition persisted, and the server-side reportSessionCost the
		// writer never saw is intact.
		var after spiceboxv1alpha1.AgentSession
		require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "skew-condpatch"}, &after))
		assert.True(t, meta.IsStatusConditionTrue(after.Status.Conditions, credCond),
			"dedup condition must persist so the ephemeral is not re-sent")
		require.NotNil(t, after.Status.EffectiveSettings)
		assert.True(t, after.Status.EffectiveSettings.ReportSessionCost,
			"operator-owned reportSessionCost must be preserved by the conditions-only patch")
	})

	t.Run("WriteOwned stamps the dedup condition against the real CRD and preserves effectiveSettings", func(t *testing.T) {
		seedSessionWithEffectiveSettings(ctx, t, env, "writeowned-cred")

		var original spiceboxv1alpha1.AgentSession
		require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "writeowned-cred"}, &original))

		mutated := original.DeepCopy()
		meta.SetStatusCondition(&mutated.Status.Conditions, metav1.Condition{
			Type:               credCond,
			Status:             metav1.ConditionTrue,
			Reason:             credReason,
			Message:            "Delivered credential link prompt covering 1 missing credential(s).",
			LastTransitionTime: metav1.Now(),
		})
		require.NoError(t, pipeline.ApplyApprovalStatusForTest(ctx, env.Client, mutated, &original),
			"WriteOwned condition write must succeed against the real CRD")

		var after spiceboxv1alpha1.AgentSession
		require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "writeowned-cred"}, &after))
		assert.True(t, meta.IsStatusConditionTrue(after.Status.Conditions, credCond),
			"CredentialRequestPublished must persist")
		require.NotNil(t, after.Status.EffectiveSettings)
		assert.True(t, after.Status.EffectiveSettings.ReportSessionCost,
			"effectiveSettings must be preserved through a WriteOwned condition write")
	})
}
