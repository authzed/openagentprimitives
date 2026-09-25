package manifests_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

func TestCertManagerBundleParses(t *testing.T) {
	groups, err := manifests.CertManager()
	require.NoError(t, err)
	require.NotEmpty(t, groups)
	total := 0
	for _, g := range groups {
		objs, err := manifests.Split(g)
		require.NoError(t, err)
		total += len(objs)
	}
	require.Greater(t, total, 10, "cert-manager bundle should contain many objects (CRDs + controller + webhook)")
}

func TestEnvoyGatewayBundleParses(t *testing.T) {
	groups, err := manifests.EnvoyGateway()
	require.NoError(t, err)
	require.NotEmpty(t, groups)
	total := 0
	for _, g := range groups {
		objs, err := manifests.Split(g)
		require.NoError(t, err)
		total += len(objs)
	}
	require.Greater(t, total, 5, "envoy-gateway bundle should contain CRDs + controller (no GatewayClass instance; oap creates the eg class separately)")
}
