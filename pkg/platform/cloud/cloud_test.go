package cloud

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

// fakeStrategy is a no-op Strategy for registry/detect tests. Real strategies
// live in pkg/platform/cloud/{gke,eks,...} and aren't linked into this test binary.
type fakeStrategy struct {
	key, prefix string
}

func (f fakeStrategy) Key() string                        { return f.key }
func (f fakeStrategy) DisplayName() string                { return f.key }
func (fakeStrategy) IsManaged() bool                      { return true }
func (f fakeStrategy) ProviderIDPrefix() string           { return f.prefix }
func (fakeStrategy) DNSEgressCIDRs() []string             { return nil }
func (fakeStrategy) GatewayBackendIngressCIDRs() []string { return nil }
func (fakeStrategy) GatewayAddressWait() (time.Duration, time.Duration) {
	return 8 * time.Minute, 7 * time.Minute
}
func (fakeStrategy) RegistryFromProviderID(string) string { return "" }
func (fakeStrategy) ProjectFromProviderID(string) string  { return "" }
func (fakeStrategy) TLS() TLSStrategy                     { return nil }
func (fakeStrategy) WorkspaceStorage() WorkspaceStorage   { return nil }
func (fakeStrategy) StatefulStorage() StatefulStorage     { return NoopStatefulStorage{} }
func (fakeStrategy) ArtifactStorage() ArtifactStorage {
	return RequireExplicitArtifactStorage{CloudName: "fake", Example: "mem://"}
}
func (fakeStrategy) SchedulingCeiling(context.Context, Clients) (schedfit.Ceiling, schedfit.Headroom, error) {
	return schedfit.Ceiling{}, schedfit.Headroom{}, nil
}
func (fakeStrategy) UnwedgeTerminatingNamespace(context.Context, Clients, Reporter, string, bool) (UnwedgeReport, error) {
	return UnwedgeReport{}, nil
}
func (fakeStrategy) EnsureGatewayController(context.Context, GatewayControllerParams) (GatewayControllerResult, error) {
	return GatewayControllerResult{Proceed: true, GatewayClass: "eg"}, nil
}
func (fakeStrategy) ResolveClusterIdentity(context.Context, ClusterIdentityParams) ClusterIdentity {
	return ClusterIdentity{}
}
func (fakeStrategy) InstallProfile() InstallProfile                 { return ProductionProfile }
func (fakeStrategy) Validate(context.Context, ValidateParams) error { return nil }

// resetRegistry clears the shared registry before a test runs so the test can
// register its own fakeStrategy fixtures against a blank slate, then restores
// the PRE-test snapshot via t.Cleanup — never a bare empty map — so no
// registration leaks into whichever test runs next, but nothing legitimately
// registered before this test (in particular the six real kinds that
// invariants_test.go's blank imports pull into this same test binary) is lost
// for good. Discarding the snapshot in Cleanup (as an earlier version of this
// helper did) left the registry permanently empty for every later test in the
// binary, once any resetRegistry-using test had run — invisible while this
// package's own tests registered only fakes, but it silently starved
// TestForRoundTripsEveryRegisteredKey (and invariants_test.go's whole-registry
// loops) of any real key to iterate, i.e. exactly the vacuous-loop failure
// mode those tests exist to catch.
func resetRegistry(t *testing.T) {
	t.Helper()
	saved := registry.byKey
	registry.byKey = map[string]Strategy{}
	t.Cleanup(func() { registry.byKey = saved })
}

func TestFor_DispatchesByKey(t *testing.T) {
	resetRegistry(t)
	Register(fakeStrategy{key: KeyDefault}, KeyDefault)
	Register(fakeStrategy{key: KeyGKE, prefix: "gce://"}, KeyGKE)

	got, err := For(KeyGKE)
	require.NoError(t, err)
	assert.Equal(t, KeyGKE, got.Key())

	_, err = For("eks")
	assert.Error(t, err, "an unregistered key must error, never silently fall back to default")
}

func TestRegister_DuplicateKeyPanics(t *testing.T) {
	resetRegistry(t)
	Register(fakeStrategy{key: "a", prefix: "gce://"}, "gke")
	assert.Panics(t, func() { Register(fakeStrategy{key: "b", prefix: "gce://"}, "gke") })
}

func TestDetect_ByProviderIDPrefix_FallsBackToDefault(t *testing.T) {
	resetRegistry(t)
	Register(fakeStrategy{key: KeyDefault}, KeyDefault)
	Register(fakeStrategy{key: KeyGKE, prefix: "gce://"}, KeyGKE)

	kc := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:       corev1.NodeSpec{ProviderID: "gce://proj/us-east1-b/n1"},
	})
	got, err := Detect(context.Background(), kc)
	require.NoError(t, err)
	assert.Equal(t, KeyGKE, got.Key())

	bare := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}})
	got, err = Detect(context.Background(), bare)
	require.NoError(t, err)
	assert.Equal(t, KeyDefault, got.Key(), "no providerID ⇒ default")
}

// TestForProviderID covers the prefix match Detect scans with, including the
// two answers that must never become a match: an empty providerID, and a kind
// that reports no prefix (the opt-in local/desktop kinds).
func TestForProviderID(t *testing.T) {
	resetRegistry(t)
	Register(fakeStrategy{key: KeyDefault}, KeyDefault)
	Register(fakeStrategy{key: KeyGKE, prefix: "gce://"}, KeyGKE)
	Register(fakeStrategy{key: KeyLocal}, KeyLocal)

	cases := []struct {
		name       string
		providerID string
		wantKey    string
		wantOK     bool
	}{
		{
			name:       "gce providerID: matched to the kind that owns the prefix",
			providerID: "gce://proj/us-east1-b/n1",
			wantKey:    KeyGKE,
			wantOK:     true,
		},
		{
			name:       "empty providerID: no match, never the prefix-less local kind",
			providerID: "",
		},
		{
			name:       "unrecognized providerID: no match, never the default kind",
			providerID: "openstack:///abc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ForProviderID(tc.providerID)
			assert.Equal(t, tc.wantOK, ok)
			if !tc.wantOK {
				assert.Nil(t, got, "a non-match returns no Strategy to accidentally use")
				return
			}
			assert.Equal(t, tc.wantKey, got.Key())
		})
	}
}

func TestForRejectsUnknownAndEmptyKeys(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		wantErr string
	}{
		{
			name:    "unknown key: error naming the registered kinds, never a silent default",
			key:     "gek",
			wantErr: `unknown cluster kind "gek"`,
		},
		{
			name:    "empty key: error, because Default() is the only route to the fallback",
			key:     "",
			wantErr: "empty cluster kind",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := For(tc.key)
			require.Error(t, err, "For(%q) must not silently downgrade to default", tc.key)
			assert.Nil(t, got, "a failed lookup must return a nil Strategy, never a usable one")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestForRoundTripsEveryRegisteredKey(t *testing.T) {
	// Every key the production binaries link must resolve to a Strategy that
	// reports that same key. A kind registered under a key it does not report
	// makes AP_CLUSTER_KIND round-tripping silently wrong.
	for _, key := range RegisteredKeys() {
		t.Run(key+": For(k).Key() == k", func(t *testing.T) {
			s, err := For(key)
			require.NoError(t, err)
			require.NotNil(t, s)
			assert.Equal(t, key, s.Key())
		})
	}
}

func TestMustForPanicsOnUnknownKey(t *testing.T) {
	assert.Panics(t, func() { MustFor("nope") },
		"MustFor is for init() and tests: a bad key there is a programming error")
}

func TestRegisterPanicsWithNoKeys(t *testing.T) {
	assert.Panics(t, func() { Register(fakeStrategy{}) },
		"the anonymous default registration is gone; every kind names its key")
}
