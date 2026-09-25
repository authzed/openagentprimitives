package useridentity

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"

	// Registers static/oauth/federated so validateSpecShape's and
	// reconcileStatus's credkindregistry.Get dispatch resolve in this
	// package's (untagged) test binary.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

func buildClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.UserIdentity{}).
		Build()
}

func reconcileOnce(t *testing.T, r *Reconciler, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Name: name}})
	require.NoError(t, err, "Reconcile")
}

func newReconciler(c client.Client) *Reconciler {
	return &Reconciler{
		Client:       c,
		APIReader:    c,
		SecretReader: adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false }),
	}
}

func TestUserIdentityValidity(t *testing.T) {
	cases := []struct {
		name       string
		objects    func() []client.Object
		wantStatus metav1.ConditionStatus
		wantReason string
		wantAvail  []string
	}{
		{
			name: "all references resolve: Valid=True/AllReferencesResolve, AvailableCredentials populated",
			objects: func() []client.Object {
				sec := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "u-x-gh", Namespace: spiceboxv1alpha1.IdentitiesNamespace},
					Data:       map[string][]byte{"token": []byte("ghp_abc")},
				}
				adoptguard.WithAdoptedLabel(sec)
				return []client.Object{
					sec,
					&spiceboxv1alpha1.UserIdentity{
						ObjectMeta: metav1.ObjectMeta{Name: "u-x"},
						Spec: spiceboxv1alpha1.UserIdentitySpec{
							Subject: "user:abc",
							Credentials: []spiceboxv1alpha1.AgentCredential{{
								Name: "gh", Type: "static",
								Static: &spiceboxv1alpha1.StaticCredentialSource{
									SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "u-x-gh", Key: "token"},
								},
							}},
						},
					},
				}
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonAllReferencesResolve,
			wantAvail:  []string{"gh"},
		},
		{
			name: "federated credential: not valid on UserIdentity, rejected before any Secret lookup: Valid=False/SpecInvalid",
			objects: func() []client.Object {
				return []client.Object{&spiceboxv1alpha1.UserIdentity{
					ObjectMeta: metav1.ObjectMeta{Name: "u-x"},
					Spec: spiceboxv1alpha1.UserIdentitySpec{
						Subject: "user:abc",
						Credentials: []spiceboxv1alpha1.AgentCredential{{
							Name: "linear", Type: "federated",
							Federated: &spiceboxv1alpha1.FederatedCredentialSource{
								Resource:          "r",
								ResourceServerURL: "https://x",
								IdPSecretRef:      spiceboxv1alpha1.SecretRef{Name: "s"},
							},
						}},
					},
				}}
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonSpecInvalid,
		},
		{
			name: "missing secret: Valid=False/SecretMissing",
			objects: func() []client.Object {
				return []client.Object{&spiceboxv1alpha1.UserIdentity{
					ObjectMeta: metav1.ObjectMeta{Name: "u-x"},
					Spec: spiceboxv1alpha1.UserIdentitySpec{
						Subject: "user:abc",
						Credentials: []spiceboxv1alpha1.AgentCredential{{
							Name: "gh", Type: "static",
							Static: &spiceboxv1alpha1.StaticCredentialSource{
								SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "u-x-gh", Key: "token"},
							},
						}},
					},
				}}
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonSecretMissing,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := buildClient(t, tc.objects()...)
			r := newReconciler(c)
			reconcileOnce(t, r, "u-x")

			var got spiceboxv1alpha1.UserIdentity
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "u-x"}, &got))
			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.UserIdentityConditionValid)
			require.NotNil(t, cond, "Valid condition must be present")
			assert.Equal(t, tc.wantStatus, cond.Status)
			assert.Equal(t, tc.wantReason, cond.Reason)
			if tc.wantStatus == metav1.ConditionTrue {
				assert.Equal(t, tc.wantAvail, got.Status.AvailableCredentials)
			}
		})
	}
}

func TestValidateSpecShape(t *testing.T) {
	cases := []struct {
		name            string
		spec            *spiceboxv1alpha1.UserIdentitySpec
		wantReason      string // "" means valid
		wantMsgContains string // if non-empty, checked against the returned message
	}{
		{
			name: "federated credential: not valid on UserIdentity per credkind.ValidOnScope: SpecInvalid/message names where it IS valid",
			spec: &spiceboxv1alpha1.UserIdentitySpec{
				Subject: "user:abc",
				Credentials: []spiceboxv1alpha1.AgentCredential{{
					Name: "linear", Type: "federated",
					Federated: &spiceboxv1alpha1.FederatedCredentialSource{
						Resource:          "r",
						ResourceServerURL: "https://x",
						IdPSecretRef:      spiceboxv1alpha1.SecretRef{Name: "s"},
					},
				}},
			},
			wantReason:      spiceboxv1alpha1.ReasonSpecInvalid,
			wantMsgContains: "is not valid here (valid on: [SessionUserIdentity])",
		},
		{
			name: "oauth credential with static block also set: SpecInvalid",
			spec: &spiceboxv1alpha1.UserIdentitySpec{
				Subject: "user:abc",
				Credentials: []spiceboxv1alpha1.AgentCredential{{
					Name: "x", Type: "oauth",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "s", Key: "k"},
					},
					OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: "s"}},
				}},
			},
			wantReason: spiceboxv1alpha1.ReasonSpecInvalid,
		},
		{
			name: "unknown credential type: rejected by the registry: SpecInvalid/message contains unknown credential type",
			spec: &spiceboxv1alpha1.UserIdentitySpec{
				Subject:     "user:abc",
				Credentials: []spiceboxv1alpha1.AgentCredential{{Name: "c", Type: "nosuch"}},
			},
			wantReason:      spiceboxv1alpha1.ReasonSpecInvalid,
			wantMsgContains: "unknown credential type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := validateSpecShape(tc.spec)
			assert.Equal(t, tc.wantReason, reason)
			if tc.wantMsgContains != "" {
				assert.Contains(t, msg, tc.wantMsgContains)
			}
		})
	}
}
