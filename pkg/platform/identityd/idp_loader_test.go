package identityd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientpkg "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/fakekind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
)

// idpLoaderScheme returns a scheme with core/v1 + spicebox v1alpha1,
// matching the scheme the loader uses to Get the CR and Secret.
func idpLoaderScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	// newScheme is defined in handlers_link_test.go (same package).
	return newScheme(t)
}

// defaultCR builds a minimal ClusterIdentityProvider with Valid=True.
func defaultCR(kind string) *spiceboxv1alpha1.ClusterIdentityProvider {
	return &spiceboxv1alpha1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:       spiceboxv1alpha1.ClusterIdentityProviderName,
			Generation: 1,
		},
		Spec: spiceboxv1alpha1.ClusterIdentityProviderSpec{
			Kind:     kind,
			ClientID: "client-id",
			ClientSecretRef: spiceboxv1alpha1.ClusterSecretKeyRef{
				Namespace: "default",
				Name:      "idp-secret",
				Key:       "clientSecret",
			},
			AllowedEmailDomains: []string{"example.com"},
		},
		Status: spiceboxv1alpha1.ClusterIdentityProviderStatus{
			Conditions: []metav1.Condition{
				{
					Type:   spiceboxv1alpha1.ConditionIdPValid,
					Status: metav1.ConditionTrue,
					Reason: spiceboxv1alpha1.ReasonIdPReady,
				},
			},
		},
	}
}

// defaultSecret returns the Secret that defaultCR references.
func defaultSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "idp-secret",
			ResourceVersion: "1",
		},
		Data: map[string][]byte{
			"clientSecret": []byte("super-secret"),
		},
	}
}

// newIdpLoader builds an idpLoader backed by a fake client pre-seeded
// with the given objects. now is an injectable clock seam.
func newIdpLoader(t *testing.T, now func() time.Time, objs ...clientpkg.Object) *idpLoader {
	t.Helper()
	scheme := idpLoaderScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &idpLoader{
		k8s:         c,
		externalURL: func() string { return "https://identityd.example.org" },
		cacheTTL:    30 * time.Second,
		now:         now,
	}
}

// countingClient wraps a client.Client and counts Get calls.
type countingClient struct {
	clientpkg.Client
	gets int
}

func (c *countingClient) Get(ctx context.Context, key clientpkg.ObjectKey, obj clientpkg.Object, opts ...clientpkg.GetOption) error {
	c.gets++
	return c.Client.Get(ctx, key, obj, opts...)
}

// failingIdPKindName is the registry name of the test-only kind below. Kept
// distinct from every production kind so registering it cannot shadow one.
const failingIdPKindName = "identityd-test-failing-new"

// failingNewIdPKind is a test-only idp.Kind whose New always fails, counting
// attempts. It stands in for the cost the loader hides: New for the oidc /
// google kinds is an issuer-discovery round trip on a 30s-timeout client, run
// while the loader's mutex is held, so an unreachable issuer makes every
// queued login pay the timeout unless the failure is remembered.
//
// attempts is written under the loader's mutex (Current holds it across
// kind.New), so no synchronization of its own is needed.
type failingNewIdPKind struct{ attempts int }

func (k *failingNewIdPKind) Name() string { return failingIdPKindName }

func (k *failingNewIdPKind) New(context.Context, idp.Config) (idp.Provider, error) {
	k.attempts++
	return nil, errors.New("issuer discovery timed out")
}

func (k *failingNewIdPKind) Wizard() idp.Wizard {
	return idp.UnavailableWizard("test-only kind")
}
func (k *failingNewIdPKind) ValidateSpec(spiceboxv1alpha1.ClusterIdentityProviderSpec) string {
	return ""
}
func (k *failingNewIdPKind) DiscoveryURL(spiceboxv1alpha1.ClusterIdentityProviderSpec) string {
	return ""
}
func (k *failingNewIdPKind) AllowedNonLocal() bool { return true }

// failingIdPKind is registered once for the whole test binary; each test
// zeroes its counter rather than re-registering (the registry has no
// unregister, and Reset would drop the production kinds other tests rely on).
var failingIdPKind = func() *failingNewIdPKind {
	k := &failingNewIdPKind{}
	registry.Register(k)
	return k
}()

// TestIdPLoaderNegativeCachesConstructionFailure pins that a failed provider
// construction is remembered: without it, every login queued behind the first
// one re-runs the same failing (and, for a real kind, 30-second) construction
// serially, because they all share one *idpLoader and the mutex is held across
// kind.New.
func TestIdPLoaderNegativeCachesConstructionFailure(t *testing.T) {
	ctx := context.Background()
	failingIdPKind.attempts = 0

	clock := time.Now()
	l := newIdpLoader(t, func() time.Time { return clock },
		defaultCR(failingIdPKindName), defaultSecret())

	for i := 0; i < 3; i++ {
		_, err := l.Current(ctx)
		require.Error(t, err, "call %d must report the construction failure", i+1)
		assert.Contains(t, err.Error(), "issuer discovery timed out",
			"the cached failure must be the real error, not a substitute")
	}
	assert.Equal(t, 1, failingIdPKind.attempts,
		"a burst of logins behind one failing IdP must construct once, not once per waiter")

	// Past the negative TTL the loader retries — a recovering IdP must not be
	// locked out for the full positive cache TTL.
	clock = clock.Add(idpNegativeCacheTTL + time.Second)
	_, err := l.Current(ctx)
	require.Error(t, err)
	assert.Equal(t, 2, failingIdPKind.attempts,
		"past the negative TTL the loader must try again")
}

func TestIdPLoader(t *testing.T) {
	ctx := context.Background()

	t.Run("no CR → ErrIdPNotConfigured", func(t *testing.T) {
		l := newIdpLoader(t, time.Now /* no objects */)
		_, err := l.Current(ctx)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrIdPNotConfigured),
			"expected ErrIdPNotConfigured, got %v", err)
	})

	t.Run("CR with Valid=False → error naming the reason, NOT ErrIdPNotConfigured", func(t *testing.T) {
		cr := defaultCR("fake")
		cr.Status.Conditions = []metav1.Condition{
			{
				Type:    spiceboxv1alpha1.ConditionIdPValid,
				Status:  metav1.ConditionFalse,
				Reason:  spiceboxv1alpha1.ReasonIdPSecretMissing,
				Message: "referenced secret not found",
			},
		}
		l := newIdpLoader(t, time.Now, cr)
		_, err := l.Current(ctx)
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrIdPNotConfigured),
			"Valid=False must not return ErrIdPNotConfigured")
		assert.Contains(t, err.Error(), "SecretMissing",
			"error must name the reason")
	})

	t.Run("CR with no Valid condition at all → error (not yet validated)", func(t *testing.T) {
		cr := defaultCR("fake")
		cr.Status.Conditions = nil // controller hasn't reconciled yet
		l := newIdpLoader(t, time.Now, cr)
		_, err := l.Current(ctx)
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrIdPNotConfigured))
		assert.Contains(t, err.Error(), "not yet been validated")
	})

	t.Run("happy: Valid=True + secret + fake kind → resolved", func(t *testing.T) {
		cr := defaultCR("fake")
		l := newIdpLoader(t, time.Now, cr, defaultSecret())
		resolved, err := l.Current(ctx)
		require.NoError(t, err)
		require.NotNil(t, resolved)
		require.NotNil(t, resolved.provider, "provider must be non-nil")
		assert.Equal(t, []string{"example.com"}, resolved.allowedDomains)
		assert.Equal(t, defaultIdPSessionTTL, resolved.sessionTTL,
			"nil spec.SessionTTL must use the 12h default")
	})

	t.Run("spec.SessionTTL 1h honored", func(t *testing.T) {
		cr := defaultCR("fake")
		cr.Spec.SessionTTL = &metav1.Duration{Duration: time.Hour}
		l := newIdpLoader(t, time.Now, cr, defaultSecret())
		resolved, err := l.Current(ctx)
		require.NoError(t, err)
		require.NotNil(t, resolved)
		assert.Equal(t, time.Hour, resolved.sessionTTL)
	})

	t.Run("secret missing despite Valid=True → error (race)", func(t *testing.T) {
		cr := defaultCR("fake")
		// No secret seeded — simulates secret deleted after reconcile.
		l := newIdpLoader(t, time.Now, cr)
		_, err := l.Current(ctx)
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrIdPNotConfigured))
		assert.Contains(t, err.Error(), "idp-secret",
			"error must name the missing secret")
	})

	t.Run("unknown kind → error naming the kind", func(t *testing.T) {
		cr := defaultCR("nonexistent-kind")
		l := newIdpLoader(t, time.Now, cr, defaultSecret())
		_, err := l.Current(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nonexistent-kind",
			"error must name the unknown kind")
		// The error message should also list what IS registered.
		assert.Contains(t, err.Error(), "registered")
	})

	t.Run("secret key absent → error naming the key", func(t *testing.T) {
		cr := defaultCR("fake")
		// Secret exists but the expected key is absent — simulates a race
		// where the key was removed after the controller validated the CR.
		secret := defaultSecret()
		delete(secret.Data, "clientSecret")
		l := newIdpLoader(t, time.Now, cr, secret)
		_, err := l.Current(ctx)
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrIdPNotConfigured))
		assert.Contains(t, err.Error(), "clientSecret",
			"error must name the missing key")
	})

	t.Run("empty external URL → error mentioning redirect", func(t *testing.T) {
		cr := defaultCR("fake")
		scheme := idpLoaderScheme(t)
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(cr, defaultSecret()).Build()
		l := &idpLoader{
			k8s:         c,
			externalURL: func() string { return "" },
			cacheTTL:    30 * time.Second,
			now:         time.Now,
		}
		_, err := l.Current(ctx)
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrIdPNotConfigured))
		assert.Contains(t, err.Error(), "redirect",
			"error must mention redirect URL derivation failure")
	})

	t.Run("cache: second Current() within TTL doesn't re-read", func(t *testing.T) {
		cr := defaultCR("fake")
		scheme := idpLoaderScheme(t)
		innerClient := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(cr, defaultSecret()).Build()
		counter := &countingClient{Client: innerClient}

		now := time.Now()
		l := &idpLoader{
			k8s:         counter,
			externalURL: func() string { return "https://identityd.example.org" },
			cacheTTL:    30 * time.Second,
			now:         func() time.Time { return now },
		}

		// First call — populates cache.
		r1, err := l.Current(ctx)
		require.NoError(t, err)
		require.NotNil(t, r1)
		getsAfterFirst := counter.gets

		// Second call — within TTL, same cache key → should not issue new Gets.
		r2, err := l.Current(ctx)
		require.NoError(t, err)
		require.NotNil(t, r2)
		// The CR and Secret reads still happen (validity + cache-key derivation
		// are always done), but kind.New is NOT re-called. We assert the Get
		// count is exactly double the first call's count — no extra Get for
		// secret or anything else.
		assert.Equal(t, getsAfterFirst, counter.gets-getsAfterFirst,
			"second call within TTL must issue the same number of Gets (CR+Secret for key), not more")
		assert.Same(t, r1, r2, "cached pointer must be returned on second call")
	})

	t.Run("cache invalidation: CR generation bump → re-resolve", func(t *testing.T) {
		cr := defaultCR("fake")
		scheme := idpLoaderScheme(t)
		innerClient := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(cr, defaultSecret()).Build()
		counter := &countingClient{Client: innerClient}

		now := time.Now()
		l := &idpLoader{
			k8s:         counter,
			externalURL: func() string { return "https://identityd.example.org" },
			cacheTTL:    30 * time.Second,
			now:         func() time.Time { return now },
		}

		// First call — populates cache.
		r1, err := l.Current(ctx)
		require.NoError(t, err)
		require.NotNil(t, r1)
		getsAfterFirst := counter.gets

		// Bump the CR's generation in the fake store to simulate an edit.
		cr.Generation = 2
		require.NoError(t, innerClient.Update(ctx, cr))

		// Second call — cache key changed because generation changed → re-resolve.
		r2, err := l.Current(ctx)
		require.NoError(t, err)
		require.NotNil(t, r2)
		// Both calls issue the same number of Gets (CR + Secret), so total Gets
		// should be exactly 2× the first call's count. More importantly, the
		// returned pointer must be freshly allocated — the cache was invalidated.
		assert.Equal(t, 2*getsAfterFirst, counter.gets,
			"generation bump must re-read (total Gets == 2× first call's Gets)")
		// The returned pointer is a freshly allocated resolved, not the same one.
		assert.NotSame(t, r1, r2, "generation bump must produce a new resolved pointer")
	})

}
