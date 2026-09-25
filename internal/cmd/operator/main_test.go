package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// TestClusterKindIsFailClosed covers the fail-closed AP_CLUSTER_KIND contract:
// an unset or unrecognized value must never fall back to a default kind — it
// is a fatal startup error, matching the MEMORY_BACKEND / ARTIFACT_STORE_URL
// selectors above.
func TestClusterKindIsFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		wantErr string
	}{
		{
			name:    "unset: fatal, never a silent default",
			kind:    "",
			wantErr: "empty cluster kind",
		},
		{
			name:    "stale/typo'd value on a live Deployment: fatal",
			kind:    "loca1",
			wantErr: `unknown cluster kind "loca1"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveClusterKindFromEnv(tc.kind)
			require.Error(t, err)
			assert.Nil(t, got)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestClusterKindResolvesLocalOnlyIdPGate asserts the cluster kind — not the
// memory backend — is what answers whether local-only-by-design IdP kinds
// (e.g. password, which has no anti-brute-force posture of its own) may
// validate.
func TestClusterKindResolvesLocalOnlyIdPGate(t *testing.T) {
	local, err := resolveClusterKindFromEnv(cloud.KeyLocal)
	require.NoError(t, err)
	assert.True(t, local.InstallProfile().AllowsLocalOnlyIdentityProviders())

	prod, err := resolveClusterKindFromEnv(cloud.KeyDefault)
	require.NoError(t, err)
	assert.False(t, prod.InstallProfile().AllowsLocalOnlyIdentityProviders(),
		"local-only IdP kinds (e.g. password) have no anti-brute-force posture of their own")
}

// TestLocalOnlyIdPGateNoLongerTracksTheMemoryBackend is the regression this
// refactor exists to prevent: the gate used to be derived as
// `backendKind != memoryBackendPostgres`, so an install running Postgres
// locally — or sqlite remotely — silently flipped an authorization gate. The
// kind and the backend are now independent inputs.
func TestLocalOnlyIdPGateNoLongerTracksTheMemoryBackend(t *testing.T) {
	local, err := resolveClusterKindFromEnv(cloud.KeyLocal)
	require.NoError(t, err)
	gke, err := resolveClusterKindFromEnv(cloud.KeyGKE)
	require.NoError(t, err)

	assert.NotEqual(t,
		local.InstallProfile().AllowsLocalOnlyIdentityProviders(),
		gke.InstallProfile().AllowsLocalOnlyIdentityProviders(),
		"the gate must follow the cluster kind")
	assert.Equal(t, "postgres", gke.InstallProfile().MemoryBackend(),
		"and the memory backend is a separate fact, not the gate's source")
}
