package settingscmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
)

func newFakeDynamic(t *testing.T) *dynfake.FakeDynamicClient {
	t.Helper()
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{aptest.ClusterAgentSettingsGVR: "ClusterAgentSettingsList"})
}

func TestSettingsApply_AppliesClusterAgentSettings(t *testing.T) {
	body := `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: ClusterAgentSettings
metadata:
  name: cluster
spec:
  defaults: {}
`
	path := aptest.WriteTempYAML(t, body)
	dyn := newFakeDynamic(t)

	prev := DynamicFactory
	defer func() { DynamicFactory = prev }()
	DynamicFactory = func(_ *apcmd.Globals) (DynIface, error) {
		return dyn, nil
	}

	out, err := runSettings(t, "apply", "-f", path)
	require.NoErrorf(t, err, "settings apply; out=%s", out)
	assert.Contains(t, out, "ClusterAgentSettings", "output should mention the kind")
	assert.Contains(t, out, "cluster", "output should mention the name")

	// Verify the object landed in the fake cluster.
	got, getErr := dyn.Resource(aptest.ClusterAgentSettingsGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	require.NoError(t, getErr, "ClusterAgentSettings 'cluster' should exist after apply")
	assert.Equal(t, "cluster", got.GetName())
}

func TestSettingsApply_RejectsNonCASKind(t *testing.T) {
	body := `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: Channel
metadata:
  name: my-channel
spec: {}
`
	path := aptest.WriteTempYAML(t, body)
	dyn := newFakeDynamic(t)

	prev := DynamicFactory
	defer func() { DynamicFactory = prev }()
	DynamicFactory = func(_ *apcmd.Globals) (DynIface, error) {
		return dyn, nil
	}

	out, err := runSettings(t, "apply", "-f", path)
	require.Errorf(t, err, "settings apply of a non-ClusterAgentSettings kind should error; out=%s", out)
	assert.Contains(t, err.Error(), "ClusterAgentSettings", "error should mention required kind")
}

func TestSettingsApply_DryRunPrintsWithoutApplying(t *testing.T) {
	body := `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: ClusterAgentSettings
metadata:
  name: cluster
spec:
  defaults: {}
`
	path := aptest.WriteTempYAML(t, body)
	dyn := newFakeDynamic(t)

	prev := DynamicFactory
	defer func() { DynamicFactory = prev }()
	DynamicFactory = func(_ *apcmd.Globals) (DynIface, error) {
		return dyn, nil
	}

	out, err := runSettings(t, "apply", "-f", path, "--dry-run")
	require.NoErrorf(t, err, "settings apply --dry-run; out=%s", out)
	assert.Contains(t, out, "ClusterAgentSettings", "dry-run output should mention the kind")
	assert.Contains(t, out, "cluster", "dry-run output should mention the name")

	// Verify the object did NOT land in the fake cluster.
	_, getErr := dyn.Resource(aptest.ClusterAgentSettingsGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	assert.Error(t, getErr, "ClusterAgentSettings should NOT exist after dry-run")
}
