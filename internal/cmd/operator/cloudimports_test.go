package main

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// TestCloudimportsRegistersClusterKinds guards internal/cmd/operator/cloudimports.go's
// blank imports — ALL SIX of them, not merely "enough that cloud.For
// resolves something". A PARTIAL cleanup (someone removes the aks/eks
// imports as "unused" since the operator's own code only ever names
// cloud.KeyLocal/cloud.KeyDefault directly) still lets cloud.For(cloud.KeyLocal)
// and cloud.For(cloud.KeyDefault) succeed — the gap only surfaces once an EKS
// or AKS cluster's `oap install` stamps AP_CLUSTER_KIND=eks/aks onto the
// Deployment and resolveClusterKindFromEnv (main.go) calls cloud.For with a
// key nothing registered. Asserting the FULL cloud.RegisteredKeys() set (not
// a subset, not just "non-empty") catches a partial cleanup, AND catches a
// future seventh cloud/<kind> package that registers a new key this file
// forgets to blank-import — either failure mode means the operator's
// fail-closed --cluster-kind/AP_CLUSTER_KIND resolution (main.go:~813)
// os.Exit(1)s on every reconcile loop start for that cloud kind, a permanent
// crash-loop indistinguishable at build/unit-test time from a healthy
// deploy — mirrors internal/cmd/webd/cloudimports_test.go for the same reason.
func TestCloudimportsRegistersClusterKinds(t *testing.T) {
	want := []string{cloud.KeyAKS, cloud.KeyDefault, cloud.KeyDesktop, cloud.KeyEKS, cloud.KeyGKE, cloud.KeyLocal}
	sort.Strings(want)
	assert.Equal(t, want, cloud.RegisteredKeys(),
		"internal/cmd/operator/cloudimports.go must blank-import every pkg/platform/cloud/<kind> package — the registered-keys set must be exactly these six, not a subset")

	for _, k := range want {
		s, err := cloud.For(k)
		require.NoError(t, err, "cloud.For(%q) must resolve (see internal/cmd/operator/cloudimports.go)", k)
		assert.Equal(t, k, s.Key())
	}
}
