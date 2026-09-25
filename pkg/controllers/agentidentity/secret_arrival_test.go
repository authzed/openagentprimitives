// pkg/controllers/agentidentity/secret_arrival_test.go
//
// Unit tests for how this reconciler converges on a credential Secret that
// arrives, or is replaced, AFTER the AgentIdentity.
//
// The operator's manager cache watches Secrets through a label filter
// (adoptguard.AdoptedLabel must Exist), and this controller's Secret watch runs
// off that filtered cache. A hand-created Secret — `kubectl create secret
// generic ...`, the documented install step — carries no such label, so it
// fires NO watch event. The label is only ever stamped BY a reconcile
// (adoptkit.AdoptSecret), so with nothing else re-enqueueing the identity the
// state is self-sustaining: no event, no reconcile, no label, no event.
//
// The reconcile-level repair is the requeue asserted below. Deleting and
// recreating a Secret to rotate a token lands in the same state, which is why
// this is not merely an install-time cosmetic.
package agentidentity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"

	// Registers static/oauth/federated/githubApp so checkCredentialSecret's
	// credkindregistry.Get dispatch resolves in this package's test binary.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

// credSecret builds the Secret a type=static credential points at. labelled
// says whether it already carries adoptguard.AdoptedLabel — i.e. whether the
// operator's label-filtered Secret cache can see it at all.
func credSecret(t *testing.T, name string, labelled bool) *corev1.Secret {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name},
		Data:       map[string][]byte{"token": []byte("demo-token-value")},
	}
	if labelled {
		adoptguard.WithAdoptedLabel(sec)
	}
	return sec
}

// reconcileWithSecret builds a reconciler over an AgentIdentity carrying one
// type=static credential, plus whatever Secrets the case supplies, and runs a
// single Reconcile. It returns the result and the reloaded AgentIdentity.
func reconcileWithSecret(t *testing.T, aiName string, objs ...client.Object) (reconcile.Result, *spiceboxv1alpha1.AgentIdentity, client.Client) {
	t.Helper()
	ai := aiWithCreds(testNS, aiName, staticCred("demo-cred", "demo-cred-secret"))
	all := append([]client.Object{ai}, objs...)
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(all...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentIdentity{}).
		Build()
	r := &Reconciler{
		Client:       c,
		APIReader:    c,
		SecretReader: adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false }),
	}
	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: testNS, Name: aiName},
	})
	require.NoError(t, err, "Reconcile must not return a transient error for this case")

	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: aiName}, &got),
		"reload the AgentIdentity the Reconcile just patched")
	return res, &got, c
}

// TestReconcile_SecretArrivalConverges covers the three states a credential's
// backing Secret can be in from the reconciler's point of view: absent,
// present-but-unadopted (invisible to the Secret watch), and present-and-adopted.
func TestReconcile_SecretArrivalConverges(t *testing.T) {
	cases := []struct {
		name         string
		secret       func(t *testing.T) *corev1.Secret // nil ⇒ no Secret at all
		wantStatus   metav1.ConditionStatus
		wantReason   string
		wantRequeue  bool
		wantAdoptedL bool // the Secret carries AdoptedLabel after the reconcile
	}{
		{
			name:        "Secret absent: Valid=False/SecretMissing, RequeueAfter > 0 (its arrival fires no watch event, so nothing else re-enqueues)",
			secret:      nil,
			wantStatus:  metav1.ConditionFalse,
			wantReason:  spiceboxv1alpha1.ReasonSecretMissing,
			wantRequeue: true,
		},
		{
			name:         "Secret present but unadopted: adopted by this reconcile, Valid=True, no requeue",
			secret:       func(t *testing.T) *corev1.Secret { return credSecret(t, "demo-cred-secret", false) },
			wantStatus:   metav1.ConditionTrue,
			wantReason:   spiceboxv1alpha1.ReasonAllReferencesResolve,
			wantRequeue:  false,
			wantAdoptedL: true,
		},
		{
			name:         "Secret present and adopted: Valid=True, no requeue",
			secret:       func(t *testing.T) *corev1.Secret { return credSecret(t, "demo-cred-secret", true) },
			wantStatus:   metav1.ConditionTrue,
			wantReason:   spiceboxv1alpha1.ReasonAllReferencesResolve,
			wantRequeue:  false,
			wantAdoptedL: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			if tc.secret != nil {
				objs = append(objs, tc.secret(t))
			}
			res, got, c := reconcileWithSecret(t, "ai-arrival", objs...)

			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid)
			require.NotNil(t, cond, "Valid condition must be present")
			assert.Equal(t, tc.wantStatus, cond.Status)
			assert.Equal(t, tc.wantReason, cond.Reason)

			if tc.wantRequeue {
				assert.Positive(t, res.RequeueAfter,
					"a missing Secret must be re-checked on a timer: its creation fires no watch event through the adoption-filtered cache, so without this the identity stays SecretMissing until the next full resync")
			} else {
				assert.Zero(t, res.RequeueAfter, "a resolved credential needs no timed re-check")
			}

			if tc.secret != nil {
				var sec corev1.Secret
				require.NoError(t, c.Get(context.Background(),
					client.ObjectKey{Namespace: testNS, Name: "demo-cred-secret"}, &sec))
				_, adopted := sec.Labels[adoptguard.AdoptedLabel]
				assert.Equal(t, tc.wantAdoptedL, adopted,
					"adoption is what puts the Secret into the label-filtered cache, so a later rotation fires a watch event")
			}
		})
	}
}
