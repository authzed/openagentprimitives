package credresolve_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
)

const (
	explainNS   = "demo-ns"
	explainName = "demo-cred-secret"
)

// liveReaderWith builds a LIVE (unfiltered) reader holding the given objects —
// the operator's uncached APIReader stand-in.
func liveReaderWith(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// explainSecret builds the credential Secret. labelled says whether it carries
// adoptguard.AdoptedLabel — i.e. whether the operator's label-filtered Secret
// cache can see it at all.
func explainSecret(labelled bool) *corev1.Secret {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: explainNS, Name: explainName},
		Data:       map[string][]byte{"token": []byte("demo-token-value")},
	}
	if labelled {
		adoptguard.WithAdoptedLabel(sec)
	}
	return sec
}

// TestExplainSecretMissing distinguishes the two conditions a read through the
// adoption-filtered Secret cache collapses into one NotFound: the Secret is
// genuinely absent, versus it is right there but carries no adoption label so
// the cache cannot see it. They call for different operator action, so they
// must read differently.
func TestExplainSecretMissing(t *testing.T) {
	otherErr := errors.New("some unrelated failure")
	missing := fmt.Errorf("%w: %s/%s", credresolve.ErrSecretMissing, explainNS, explainName)

	cases := []struct {
		name string
		live func(t *testing.T) client.Reader
		in   error
		// check asserts on the returned error.
		check func(t *testing.T, got error)
	}{
		{
			name: "nil error: passed through untouched (there is nothing to explain)",
			live: func(t *testing.T) client.Reader { return liveReaderWith(t, explainSecret(false)) },
			in:   nil,
			check: func(t *testing.T, got error) {
				assert.NoError(t, got)
			},
		},
		{
			name: "unrelated error: passed through untouched, never reclassified as an adoption problem",
			live: func(t *testing.T) client.Reader { return liveReaderWith(t, explainSecret(false)) },
			in:   otherErr,
			check: func(t *testing.T, got error) {
				assert.Equal(t, otherErr, got)
				assert.False(t, errors.Is(got, credresolve.ErrSecretNotAdopted))
			},
		},
		{
			name: "no live reader: stays ErrSecretMissing (nothing can distinguish the two states)",
			live: func(t *testing.T) client.Reader { return nil },
			in:   missing,
			check: func(t *testing.T, got error) {
				assert.ErrorIs(t, got, credresolve.ErrSecretMissing)
				assert.False(t, errors.Is(got, credresolve.ErrSecretNotAdopted))
			},
		},
		{
			name: "Secret genuinely absent: stays ErrSecretMissing",
			live: func(t *testing.T) client.Reader { return liveReaderWith(t) },
			in:   missing,
			check: func(t *testing.T, got error) {
				assert.ErrorIs(t, got, credresolve.ErrSecretMissing)
				assert.False(t, errors.Is(got, credresolve.ErrSecretNotAdopted),
					"an absent Secret is not an adoption problem")
			},
		},
		{
			name: "Secret present but unadopted: ErrSecretNotAdopted, message names the Secret and the adoption label",
			live: func(t *testing.T) client.Reader { return liveReaderWith(t, explainSecret(false)) },
			in:   missing,
			check: func(t *testing.T, got error) {
				require.Error(t, got)
				assert.ErrorIs(t, got, credresolve.ErrSecretNotAdopted,
					"a Secret that is right there must not be reported as missing")
				assert.False(t, errors.Is(got, credresolve.ErrSecretMissing),
					"the two conditions must be distinguishable by errors.Is, not only by prose")
				assert.Contains(t, got.Error(), explainNS+"/"+explainName)
				assert.Contains(t, got.Error(), adoptguard.AdoptedLabel,
					"the message must name the label whose absence hides the Secret")
			},
		},
		{
			name: "Secret present and already adopted: stays ErrSecretMissing (the filtered cache is merely lagging)",
			live: func(t *testing.T) client.Reader { return liveReaderWith(t, explainSecret(true)) },
			in:   missing,
			check: func(t *testing.T, got error) {
				assert.ErrorIs(t, got, credresolve.ErrSecretMissing)
				assert.False(t, errors.Is(got, credresolve.ErrSecretNotAdopted),
					"an adopted Secret is not an adoption problem")
			},
		},
		{
			name: "live read itself fails: stays ErrSecretMissing and the classify failure is named, never dropped",
			live: func(t *testing.T) client.Reader {
				s := runtime.NewScheme()
				require.NoError(t, scheme.AddToScheme(s))
				return fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						return errors.New("apiserver unreachable")
					},
				}).Build()
			},
			in: missing,
			check: func(t *testing.T, got error) {
				assert.ErrorIs(t, got, credresolve.ErrSecretMissing)
				assert.Contains(t, got.Error(), "apiserver unreachable",
					"a classification read that failed must be reported, not swallowed")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := credresolve.ExplainSecretMissing(context.Background(), tc.live(t),
				explainNS, explainName, tc.in)
			tc.check(t, got)
		})
	}
}

// TestExplainSecretMissing_ReadsMetadataOnly pins that the classification never
// pulls a non-adopted Secret's DATA into the operator. Reading the value bytes
// of a Secret the operator has not adopted is exactly the overreach the
// adoption label filter exists to prevent, so the probe must ask for metadata.
func TestExplainSecretMissing_ReadsMetadataOnly(t *testing.T) {
	s := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(s))

	var sawKinds []string
	live := fake.NewClientBuilder().WithScheme(s).
		WithObjects(explainSecret(false)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				sawKinds = append(sawKinds, fmt.Sprintf("%T", obj))
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()

	got := credresolve.ExplainSecretMissing(context.Background(), live, explainNS, explainName,
		fmt.Errorf("%w: %s/%s", credresolve.ErrSecretMissing, explainNS, explainName))
	require.ErrorIs(t, got, credresolve.ErrSecretNotAdopted)
	assert.Equal(t, []string{"*v1.PartialObjectMetadata"}, sawKinds,
		"the probe must be metadata-only: a typed Secret Get would pull the value bytes of an object the operator has not adopted")
}
