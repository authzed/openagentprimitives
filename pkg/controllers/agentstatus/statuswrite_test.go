package agentstatus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func writeOwnedScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

// sessionWithEffectiveSettings builds a channel-attached AgentSession whose
// status carries an operator-owned effectiveSettings snapshot (including the
// required reportSessionCost field). This mirrors a real parked session: the
// operator has stamped effectiveSettings, and channelsd is about to add its
// own approval-owned condition.
func sessionWithEffectiveSettings(t *testing.T) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess", Namespace: "default", ResourceVersion: "1"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials,
			EffectiveSettings: &spiceboxv1alpha1.EffectiveSettings{
				Model:             spiceboxv1alpha1.ModelConfig{Name: "claude-sonnet-5", Provider: "anthropic"},
				ReportSessionCost: true,
			},
			Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.AgentSessionConditionClassResolved,
				Status:             metav1.ConditionTrue,
				Reason:             "AllReferencesResolve",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// TestWriteOwned_ConditionWrite_DoesNotResendOperatorOwnedFields is the
// regression test for the credential-request ephemeral re-send loop.
//
// Root cause: channelsd's dedup-condition write went through
// agentstatus.WriteOwned's condition path, which did a full Status().Update of
// a reloaded AgentSession. A full Update re-serializes the ENTIRE status,
// including operator-owned effectiveSettings. When channelsd's compiled types
// lag the CRD by a required field (e.g. effectiveSettings.reportSessionCost),
// the Get->Update round-trip silently drops it and the apiserver rejects the
// whole write with "reportSessionCost: Required value" — so the
// CredentialRequestPublished dedup condition never persists and the ephemeral
// re-sends every tick.
//
// The invariant: a writer changing only its owned conditions must issue a
// surgical patch scoped to conditions, never a full-object Update that carries
// (and can be rejected on) fields it does not own.
func TestWriteOwned_ConditionWrite_DoesNotResendOperatorOwnedFields(t *testing.T) {
	sch := writeOwnedScheme(t)
	original := sessionWithEffectiveSettings(t)

	var (
		fullUpdateCalled bool
		conditionPatch   []byte
	)
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		WithObjects(original).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if sub == "status" {
					fullUpdateCalled = true
				}
				return cl.Status().Update(ctx, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if sub == "status" {
					data, derr := patch.Data(obj)
					require.NoError(t, derr, "compute patch bytes")
					conditionPatch = append([]byte(nil), data...)
				}
				return cl.Status().Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	// channelsd adds its approval-owned dedup condition to a copy of the
	// session it read (original), then persists via WriteOwned(OwnerApprovals).
	sess := original.DeepCopy()
	meta.SetStatusCondition(&sess.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.AgentSessionConditionCredentialRequestPublished,
		Status:  metav1.ConditionTrue,
		Reason:  spiceboxv1alpha1.ReasonCredentialRequestPublished,
		Message: "Delivered credential link prompt covering 1 missing credential(s).",
	})

	require.NoError(t, WriteOwned(context.Background(), c, sess, original, spiceboxv1alpha1.OwnerApprovals),
		"WriteOwned must persist the owned condition")

	// The condition write must NOT be a full-object status Update: a full
	// Update re-serializes operator-owned effectiveSettings and is what makes
	// the write fragile to version skew on a required field.
	assert.False(t, fullUpdateCalled,
		"condition write must be a surgical patch, not a full Status().Update that re-serializes operator-owned fields")

	// It must be a status patch whose body touches only the conditions array —
	// never effectiveSettings (an operator-owned field channelsd must not write).
	require.NotEmpty(t, conditionPatch, "condition write must go out as a status patch")
	assert.NotContains(t, string(conditionPatch), "effectiveSettings",
		"condition patch must not carry operator-owned effectiveSettings")
	assert.Contains(t, string(conditionPatch), "CredentialRequestPublished",
		"condition patch must carry the new condition")

	// And the condition must actually be persisted (dedup guard sticks).
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(original), &got))
	assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionCredentialRequestPublished),
		"CredentialRequestPublished dedup condition must persist so the ephemeral is not re-sent")
	require.NotNil(t, got.Status.EffectiveSettings, "operator-owned effectiveSettings must be preserved")
	assert.True(t, got.Status.EffectiveSettings.ReportSessionCost,
		"operator-owned effectiveSettings.reportSessionCost must be preserved by a condition write")
}
