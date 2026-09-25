// pkg/platform/cloud/invariants_test.go
//
// Invariants asserted over the WHOLE registry rather than over one branch, so a
// kind added later cannot violate them by copying the wrong profile.
package cloud_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/aks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/desktop"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/eks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged"
)

// nodeWithProviderID returns a single-node fixture carrying the given
// providerID. Duplicated from local/validate_test.go's helper of the same name
// (a different Go package, so no redeclaration) rather than imported: an
// unexported test helper with no shared production home.
func nodeWithProviderID(id string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:       corev1.NodeSpec{ProviderID: id},
	}
}

// THE first-boot invariant.
//
// SpiceDB's --datastore-bootstrap-files applies only to an EMPTY datastore and
// is fatal against a non-empty one — safe exactly when the datastore is
// ephemeral, process-local and single-replica, which is the memory engine and
// only the memory engine.
//
// Against a shared datastore it is doubly fatal: at N>1 replicas one replica
// wins the race and the rest crashloop PERMANENTLY (the loser's killer, a
// non-empty datastore, was created by its own sibling and never goes away), and
// even at one replica the pod dies on its first restart, taking authorization
// down cluster-wide long after the install that planted it. The local-dev path
// uses an in-memory datastore that genuinely IS empty on every boot, so the
// fault is invisible in the mode everyone tests — hence asserting over every
// registered kind rather than one branch.
//
// The property is opt-in-only-ness, NOT "the key must be cloud.KeyLocal": a
// kind can safely never land on shared, persistent, multi-replica
// infrastructure exactly when (a) it reports no ProviderIDPrefix, so
// cloud.Detect can never select it, and (b) it is not cloud.KeyDefault, so it
// is never the fallback. Pinning a key instead would stop generalizing the
// moment a second opt-in kind (desktop) shared the ephemeral datastore.
func TestEphemeralDatastoreKindsAreOptInOnly(t *testing.T) {
	keys := cloud.RegisteredKeys()
	require.NotEmpty(t, keys, "the registry must not be empty — a missing blank import would make this loop vacuous")
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			s := cloud.MustFor(key)
			if s.InstallProfile().SpiceDBDatastore() == cloud.DatastoreMemory {
				assert.Empty(t, s.ProviderIDPrefix(),
					"a kind with an ephemeral SpiceDB datastore must report no ProviderIDPrefix, "+
						"so cloud.Detect can never select it — bootstrap-files against a shared/persistent datastore crashloops permanently")
				assert.NotEqual(t, cloud.KeyDefault, key,
					"a kind with an ephemeral SpiceDB datastore must never be cloud.KeyDefault — "+
						"it must never be the fallback for an undetected cluster")
			}
		})
	}
}

// A kind that runs an ephemeral datastore must not also claim durable storage
// semantics — the two together are the shape that shipped the outage.
func TestEphemeralDatastoreImpliesNoHostnameRequirement(t *testing.T) {
	keys := cloud.RegisteredKeys()
	require.NotEmpty(t, keys, "the registry must not be empty — a missing blank import would make this loop vacuous")
	ran := 0
	for _, key := range keys {
		s := cloud.MustFor(key)
		p := s.InstallProfile()
		if p.SpiceDBDatastore() != cloud.DatastoreMemory {
			continue
		}
		ran++
		t.Run(key+": is a coherent developer profile", func(t *testing.T) {
			assert.True(t, p.UsesLocalDevImages())
			assert.False(t, p.RequiresExternalHostname())
		})
	}
	assert.NotZero(t, ran, "at least one registered dev-family kind (local, desktop) must use the ephemeral datastore, or this test never actually runs its subtest")
}

func TestDetectNeverReturnsAnEphemeralDatastoreKind(t *testing.T) {
	// No nodes, GKE nodes, and unrecognized nodes must all avoid EVERY opt-in
	// dev-family kind (local, desktop, and any future one sharing the memory
	// datastore) — not merely `local` by name. Asserting the KIND KEY here
	// would stop generalizing the moment a second opt-in kind existed;
	// asserting the DATASTORE FACT covers any kind that ever carries an
	// ephemeral SpiceDB datastore, present or future.
	for _, providerID := range []string{"", "gce://p/z/n", "aws:///z/i-0", "azure:///s/rg/x", "weird://x"} {
		t.Run("providerID="+providerID, func(t *testing.T) {
			kc := fake.NewSimpleClientset(nodeWithProviderID(providerID))
			got, err := cloud.Detect(context.Background(), kc)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.NotEqual(t, cloud.DatastoreMemory, got.InstallProfile().SpiceDBDatastore(),
				"Detect must never select a kind with an ephemeral SpiceDB datastore: it is opt-in only")
		})
	}
}

func TestEveryKindImplementsTheFullStrategy(t *testing.T) {
	// Guards against a kind added with a nil sub-interface, which would panic on
	// first use rather than fail at registration.
	keys := cloud.RegisteredKeys()
	require.NotEmpty(t, keys, "the registry must not be empty — a missing blank import would make this loop vacuous")
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			s := cloud.MustFor(key)
			assert.NotNil(t, s.TLS())
			assert.NotNil(t, s.WorkspaceStorage())
			assert.NotNil(t, s.StatefulStorage())
			assert.NotNil(t, s.ArtifactStorage())
			assert.NotNil(t, s.InstallProfile())
			assert.NotEmpty(t, s.Key())
			assert.NotEmpty(t, s.DisplayName())
		})
	}
}

func TestNeedsBundledPostgresMatchesTheProfile(t *testing.T) {
	keys := cloud.RegisteredKeys()
	require.NotEmpty(t, keys, "the registry must not be empty — a missing blank import would make this loop vacuous")
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			s := cloud.MustFor(key)
			p := s.InstallProfile()
			want := p.SpiceDBDatastore() == cloud.DatastorePostgres || p.MemoryBackend() == "postgres"
			assert.Equal(t, want, cloud.NeedsBundledPostgres(s))
		})
	}
}
