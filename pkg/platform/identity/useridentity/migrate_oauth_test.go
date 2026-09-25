package useridentity

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
)

const migrateMaster = "u-hash-linear-oauth"

// coLocatedMaster is a master Secret in the pre-split shape: tokens AND
// redemption material in one object.
func coLocatedMaster() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: migrateMaster, Namespace: spiceboxv1alpha1.IdentitiesNamespace, UID: "master-uid",
		},
		Data: map[string][]byte{
			"access_token":   []byte("at"),
			"refresh_token":  []byte("rt"),
			"token_endpoint": []byte("https://idp.example.invalid/token"),
			"client_id":      []byte("cid"),
			"client_secret":  []byte("cs"),
		},
	}
}

func migrateTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// TestMigrateRedemptionMaterial sweeps the shapes a master can be in when the
// reconciler reaches it. The invariant across all of them: the material is
// readable from SOMEWHERE at every point — the migration may leave both copies,
// never neither.
func TestMigrateRedemptionMaterial(t *testing.T) {
	sibName := refresh.MaterialSecretName(migrateMaster)
	cases := []struct {
		name      string
		objects   func() []client.Object
		wantMoved bool
		wantErr   bool
		check     func(t *testing.T, c client.Client)
	}{
		{
			name:      "pre-split master: material moves to the sibling and is stripped from the master",
			objects:   func() []client.Object { return []client.Object{coLocatedMaster()} },
			wantMoved: true,
			check: func(t *testing.T, c client.Client) {
				master := getSecret(t, c, migrateMaster)
				for _, k := range redemptionKeys {
					assert.NotContainsf(t, master.Data, k, "%q must not survive on the master", k)
				}
				assert.Equal(t, []byte("rt"), master.Data["refresh_token"], "tokens stay put")
				sib := getSecret(t, c, sibName)
				assert.Equal(t, []byte("cid"), sib.Data["client_id"])
				assert.Equal(t, []byte("cs"), sib.Data["client_secret"])
				assert.Contains(t, sib.Labels, refresh.MaterialSecretLabel)
				assert.Contains(t, sib.Labels, adoptguard.AdoptedLabel)
				require.Len(t, sib.OwnerReferences, 1)
				assert.Equal(t, migrateMaster, sib.OwnerReferences[0].Name)
			},
		},
		{
			name: "already-split master: nothing to move, no sibling minted",
			objects: func() []client.Object {
				m := coLocatedMaster()
				for _, k := range redemptionKeys {
					delete(m.Data, k)
				}
				return []client.Object{m}
			},
			wantMoved: false,
			check: func(t *testing.T, c client.Client) {
				var sib corev1.Secret
				err := c.Get(context.Background(), client.ObjectKey{
					Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: sibName,
				}, &sib)
				assert.True(t, apierrors.IsNotFound(err),
					"a credential with no material must not get an empty sibling")
			},
		},
		{
			name:      "master does not exist: reports nothing to move, not an error",
			objects:   func() []client.Object { return nil },
			wantMoved: false,
		},
		{
			name: "another credential's master at the sibling name: refused, master keeps its material",
			objects: func() []client.Object {
				return []client.Object{coLocatedMaster(), &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name: sibName, Namespace: spiceboxv1alpha1.IdentitiesNamespace,
					},
					Data: map[string][]byte{"access_token": []byte("collider-at")},
				}}
			},
			wantMoved: false,
			wantErr:   true,
			check: func(t *testing.T, c client.Client) {
				master := getSecret(t, c, migrateMaster)
				assert.Contains(t, master.Data, "token_endpoint",
					"a refused migration must leave the credential refreshable")
				assert.Equal(t, []byte("collider-at"), getSecret(t, c, sibName).Data["access_token"],
					"the colliding credential must be untouched")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := migrateTestClient(t, tc.objects()...)
			moved, err := MigrateRedemptionMaterial(context.Background(), c,
				spiceboxv1alpha1.IdentitiesNamespace, migrateMaster)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantMoved, moved, "moved")
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}

// TestMigrateRedemptionMaterial_KeepsTheMasterWhenTheSiblingWriteFails pins the
// ordering, which is the whole safety argument. The redemption material's only
// durable home is these two Secrets — a DCR-minted client_id/client_secret and
// the discovered token_endpoint live otherwise only in the authorization flow's
// single-use, in-process state. Stripping the master before the sibling write
// has returned would therefore destroy the credential outright on any failure,
// recoverable only by a manual re-link.
func TestMigrateRedemptionMaterial_KeepsTheMasterWhenTheSiblingWriteFails(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(coLocatedMaster()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object,
				opts ...client.CreateOption) error {
				return apierrors.NewInternalError(fmt.Errorf("etcd unavailable"))
			},
		}).Build()

	moved, err := MigrateRedemptionMaterial(ctx, c, spiceboxv1alpha1.IdentitiesNamespace, migrateMaster)

	require.Error(t, err, "a failed sibling write must be reported, not swallowed")
	assert.False(t, moved)
	master := getSecret(t, c, migrateMaster)
	for _, k := range redemptionKeys {
		assert.Containsf(t, master.Data, k,
			"%q is the only remaining copy; stripping it before the sibling is durable loses the credential", k)
	}
}

func getSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: name,
	}, &sec))
	return &sec
}
