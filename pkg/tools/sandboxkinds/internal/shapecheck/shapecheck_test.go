package shapecheck_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/internal/shapecheck"
)

// The whole point: a backend with no Kubernetes client at all must be able to
// satisfy sandboxkinds.Kind. If this stops compiling, a Kubernetes assumption
// has leaked into the seam.
func TestKind_NeedsNoKubernetesClient(t *testing.T) {
	rt, err := shapecheck.Kind{}.NewRuntime(sandboxkinds.Deps{})
	require.NoError(t, err, "an off-cluster backend must construct without a client")
	require.NotNil(t, rt)
}

// An off-cluster backend cannot mount a ConfigMap or share a Kubernetes PVC,
// but it CAN share a workspace inside its own storage world. That is a real
// domain, not an absence of one.
func TestKind_CapabilitiesAndDomain(t *testing.T) {
	k := shapecheck.Kind{}
	assert.True(t, k.Supports(sandboxkinds.FeatureSharedWorkspace))
	assert.False(t, k.Supports(sandboxkinds.FeatureConfigMapMounts))
	assert.False(t, k.Supports(sandboxkinds.FeatureUnpackMounts),
		"an off-cluster backend has no initContainer to expand an archive with")
	assert.True(t, k.Supports(sandboxkinds.FeatureHostEgressAllowlist))
	assert.Equal(t, "shapecheck-volume", k.WorkspaceDomain())
	assert.NotEqual(t, "kubernetes-pvc", k.WorkspaceDomain(),
		"an off-cluster backend must not claim the Kubernetes storage domain")
}

// The handle is a bare opaque ID — no namespace, no name pair, nothing
// pod-shaped.
func TestRuntime_HandleIsABareID(t *testing.T) {
	rt, err := shapecheck.Kind{}.NewRuntime(sandboxkinds.Deps{})
	require.NoError(t, err)

	h, err := rt.Ensure(context.Background(), sandboxkinds.EnsureRequest{
		Session: shapecheck.DemoSession(), Class: shapecheck.DemoClass(),
	})
	require.NoError(t, err)
	assert.Equal(t, "shapecheck", h.Kind)
	assert.NotContains(t, h.Ref, "/", "an off-cluster handle must not be namespace/name shaped")
}

// A backend with nothing cluster-local to observe contributes no watches.
func TestRuntime_ContributesNoWatches(t *testing.T) {
	rt, err := shapecheck.Kind{}.NewRuntime(sandboxkinds.Deps{})
	require.NoError(t, err)
	assert.Empty(t, rt.Watches())
}

// The optional interfaces must be reachable by type assertion, with no
// parallel capability flag to keep in sync.
func TestRuntime_ImplementsOptionalInterfaces(t *testing.T) {
	rt, err := shapecheck.Kind{}.NewRuntime(sandboxkinds.Deps{})
	require.NoError(t, err)

	ft, ok := rt.(sandboxkinds.FileTransferer)
	require.True(t, ok, "a backend with a native file API must satisfy FileTransferer")

	h, err := rt.Ensure(context.Background(), sandboxkinds.EnsureRequest{
		Session: shapecheck.DemoSession(), Class: shapecheck.DemoClass(),
	})
	require.NoError(t, err)

	require.NoError(t, ft.PutFile(context.Background(), h, "/w/x", []byte("hello")))
	got, err := ft.GetFile(context.Background(), h, "/w/x")
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), got)

	_, ok = rt.(sandboxkinds.Snapshotter)
	assert.True(t, ok, "a backend with native snapshots must satisfy Snapshotter")
}
