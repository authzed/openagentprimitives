// pkg/platform/identity/broker/inproc/notadopted_test.go
//
// The operator wires this broker over the MANAGER's client, whose Secret
// informer is label-filtered to objects the operator has adopted. A Secret an
// operator recreated by hand — rotating a token — comes back without that
// label, so every read through that client returns NotFound and the resolve
// reports "secret missing" about a Secret sitting right there. These tests pin
// the LiveReader classification that tells the two apart.
package inproc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
)

const (
	adoptNS         = "agent-ns"
	adoptSecretName = "demo-cred-secret"
)

// adoptionFilteredClient models the operator's manager client: its Secret
// informer runs a label-filtered watch, so a Secret without the adoption label
// is simply not there as far as this client is concerned.
func adoptionFilteredClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if _, isSecret := obj.(*corev1.Secret); !isSecret {
					return nil
				}
				if _, adopted := obj.GetLabels()[adoptguard.AdoptedLabel]; !adopted {
					return apierrors.NewNotFound(
						schema.GroupResource{Resource: "secrets"}, key.Name)
				}
				return nil
			},
		}).Build()
}

// adoptSecret builds the credential's backing Secret, labelled or not.
func adoptSecret(labelled bool) *corev1.Secret {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: adoptNS, Name: adoptSecretName},
		Data:       map[string][]byte{"token": []byte("demo-token-value")},
	}
	if labelled {
		adoptguard.WithAdoptedLabel(sec)
	}
	return sec
}

// TestBroker_ClassifiesAFilteredCacheMiss covers the three Secret states behind
// one NotFound from the adoption-filtered client: absent, present-but-unadopted,
// present-and-adopted.
func TestBroker_ClassifiesAFilteredCacheMiss(t *testing.T) {
	cases := []struct {
		name string
		// secret is what the cluster actually holds; nil means no Secret at all.
		secret   *corev1.Secret
		withLive bool
		check    func(t *testing.T, value string, err error)
	}{
		{
			name:     "Secret absent, live reader wired: ErrSecretMissing stands",
			secret:   nil,
			withLive: true,
			check: func(t *testing.T, _ string, err error) {
				require.Error(t, err)
				assert.ErrorIs(t, err, credresolve.ErrSecretMissing)
				assert.NotErrorIs(t, err, credresolve.ErrSecretNotAdopted)
			},
		},
		{
			name:     "Secret present but unadopted, live reader wired: ErrSecretNotAdopted names the real condition",
			secret:   adoptSecret(false),
			withLive: true,
			check: func(t *testing.T, _ string, err error) {
				require.Error(t, err)
				assert.ErrorIs(t, err, credresolve.ErrSecretNotAdopted,
					"a token rotation that recreates the Secret drops the adoption label; reporting that as 'missing' sends the operator looking for a Secret that is right there")
				assert.NotErrorIs(t, err, credresolve.ErrSecretMissing)
				assert.Contains(t, err.Error(), adoptNS+"/"+adoptSecretName)
			},
		},
		{
			name:     "Secret present but unadopted, NO live reader: stays ErrSecretMissing (nothing can tell the difference)",
			secret:   adoptSecret(false),
			withLive: false,
			check: func(t *testing.T, _ string, err error) {
				require.Error(t, err)
				assert.ErrorIs(t, err, credresolve.ErrSecretMissing)
				assert.NotErrorIs(t, err, credresolve.ErrSecretNotAdopted)
			},
		},
		{
			name:     "Secret present and adopted: resolves normally, classification never runs",
			secret:   adoptSecret(true),
			withLive: true,
			check: func(t *testing.T, value string, err error) {
				require.NoError(t, err)
				assert.Equal(t, "demo-token-value", value)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			if tc.secret != nil {
				objs = append(objs, tc.secret)
			}
			b := New(adoptionFilteredClient(t, objs...))
			if tc.withLive {
				// The operator's uncached APIReader: unfiltered, so it sees a
				// Secret the manager cache cannot.
				b.LiveReader = fake.NewClientBuilder().
					WithScheme(testfixtures.NewScheme(t)).
					WithObjects(objs...).Build()
			}

			res, err := b.Resolve(t.Context(), broker.Request{
				Credentials: []spiceboxv1alpha1.CredentialDescriptor{
					staticDesc(adoptNS, adoptSecretName, "token", "DEMO_TOKEN"),
				},
			})
			tc.check(t, res.EnvVars["DEMO_TOKEN"], err)
		})
	}
}
