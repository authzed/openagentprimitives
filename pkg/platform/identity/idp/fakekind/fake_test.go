package fakekind_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/fakekind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
)

// TestFakeKindRegistered verifies the init() registered the kind.
func TestFakeKindRegistered(t *testing.T) {
	k, ok := registry.Get("fake")
	require.True(t, ok, "fake kind must be registered")
	assert.Equal(t, "fake", k.Name())
}

// TestValidateSpec verifies the fake (test-only) kind has no spec rule.
func TestValidateSpec(t *testing.T) {
	k := &fakekind.Kind{}
	msg := k.ValidateSpec(spiceboxv1alpha1.ClusterIdentityProviderSpec{Kind: "fake"})
	assert.Empty(t, msg)
}

// TestDiscoveryURL verifies the fake kind needs no remote discovery probe.
func TestDiscoveryURL(t *testing.T) {
	k := &fakekind.Kind{}
	got := k.DiscoveryURL(spiceboxv1alpha1.ClusterIdentityProviderSpec{Kind: "fake"})
	assert.Empty(t, got)
}

// TestAllowedNonLocal verifies the fake (test-only) kind is treated as
// admissible on a non-local cluster — it is never used in a real
// deployment, so it carries no local-only brute-force exposure of its own.
func TestAllowedNonLocal(t *testing.T) {
	k := &fakekind.Kind{}
	assert.True(t, k.AllowedNonLocal())
}
