package main

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// TestCloudimportsRegistersClusterKinds guards internal/cmd/webd/cloudimports.go's
// blank imports — ALL SIX of them, not merely "enough that cloud.For
// resolves something". A PARTIAL cleanup (someone removes the aks/eks/gke
// imports as "unused" and leaves local/unmanaged alone because a test name
// mentions them) still lets cloud.For(cloud.KeyLocal)/cloud.For(cloud.KeyDefault)
// succeed — an earlier version of this test asserted only those two and
// stayed green under exactly that partial removal. Asserting the FULL
// cloud.RegisteredKeys() set (not a subset, not just "non-empty") catches a
// partial cleanup, AND catches a future seventh cloud/<kind> package that
// registers a new key this file forgets to blank-import — either failure
// mode means run()'s fail-closed --cluster-kind resolution can refuse to
// start webd for kinds that should be valid. See
// internal/cmd/webd/cloudimports.go's doc comment for the full story.
func TestCloudimportsRegistersClusterKinds(t *testing.T) {
	want := []string{cloud.KeyAKS, cloud.KeyDefault, cloud.KeyDesktop, cloud.KeyEKS, cloud.KeyGKE, cloud.KeyLocal}
	sort.Strings(want)
	assert.Equal(t, want, cloud.RegisteredKeys(),
		"internal/cmd/webd/cloudimports.go must blank-import every pkg/platform/cloud/<kind> package — the registered-keys set must be exactly these six, not a subset")

	for _, k := range want {
		s, err := cloud.For(k)
		require.NoError(t, err, "cloud.For(%q) must resolve (see internal/cmd/webd/cloudimports.go)", k)
		assert.Equal(t, k, s.Key())
	}

	// The two InstallProfile facts webd's run() actually branches on. `local`
	// and `desktop` are, as of this branch, the first pair of registered kinds
	// whose ServesLocalWebChat() and AllowsSharedOrigin() genuinely diverge
	// for the SAME kind (local: false/true) — before desktop existed, every
	// registered kind answered the same value for both methods, so no
	// behavioral test (as opposed to internal/cmd/webd/sharedorigin_wiring_test.go's
	// source scan) could catch run() reading the wrong one for the other. This
	// pins that divergence directly: swap the two InstallProfile getters back
	// in run() (or in pkg/platform/cloud/profile.go) and these assertions fail.
	local, err := cloud.For(cloud.KeyLocal)
	require.NoError(t, err)
	assert.False(t, local.InstallProfile().ServesLocalWebChat(),
		"local (--local's public ngrok tunnel) must not serve the single-user built-in chat")
	assert.True(t, local.InstallProfile().AllowsSharedOrigin(),
		"local's single-tunnel dev flow still shares one origin — this must NOT track ServesLocalWebChat")

	desktop, err := cloud.For(cloud.KeyDesktop)
	require.NoError(t, err)
	assert.True(t, desktop.InstallProfile().ServesLocalWebChat(),
		"desktop (oap desktop's network-confined VM) may serve the single-user built-in chat")
	assert.True(t, desktop.InstallProfile().AllowsSharedOrigin())

	def, err := cloud.For(cloud.KeyDefault)
	require.NoError(t, err)
	assert.False(t, def.InstallProfile().ServesLocalWebChat())
}
