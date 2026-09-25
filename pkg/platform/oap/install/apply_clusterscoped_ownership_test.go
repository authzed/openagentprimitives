package install

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// stampedClusterScoped builds a live cluster-scoped object carrying the install
// labels a previous install of `name` into `ns` would have left on it. Passing
// ns="" models an object stamped before the namespace label existed.
func stampedClusterScoped(kind, name, installName, installNs string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("agentprimitives.authzed.com/v1alpha1")
	obj.SetKind(kind)
	obj.SetName(name)
	labels := map[string]string{instance.LabelInstall: installName}
	if installNs != "" {
		labels[instance.LabelInstallNamespace] = installNs
	}
	obj.SetLabels(labels)
	return obj
}

// A cluster-scoped object has no namespace of its own, so the ONLY thing
// separating one install's SpiceboxToolspec from another's was the install
// name — a value the caller picks. An install run under a name that happens to
// match therefore read as "ours from a prior run", returned before the
// cluster-scoped refusal could ever see it, and force-applied over the other
// install's spec with no conflict raised and nothing logged.
//
// Namespace is what RBAC actually gates, so the labels now carry it too, and a
// cluster-scoped object stamped for another namespace is a conflict — which for
// a cluster-scoped object means a hard refusal, since resolveConflicts never
// lets one be adopted.
func TestCheckResourceOwnership_ClusterScoped_NameMatchFromAnotherNamespaceIsAConflict(t *testing.T) {
	live := stampedClusterScoped("SpiceboxToolspec", "shared-spec", "acme", "tenant-a")
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(live).Build()

	bundled := stampedClusterScoped("SpiceboxToolspec", "shared-spec", "acme", "tenant-b")

	adopt, conflict, err := checkResourceOwnership(context.Background(), c, bundled, "acme", "tenant-b", true, false)

	require.NoError(t, err)
	assert.False(t, adopt)
	require.NotNil(t, conflict, "another namespace's cluster-scoped object must not read as ours")
	assert.True(t, conflict.ClusterScoped,
		"reported cluster-scoped, which is what makes resolveConflicts refuse it outright")
}

// The same install re-run from its own namespace still converges. Without this
// the tightening would break every legitimate re-install, which is the way a
// guard like this gets reverted rather than fixed.
func TestCheckResourceOwnership_ClusterScoped_SameInstallSameNamespaceConverges(t *testing.T) {
	live := stampedClusterScoped("SpiceboxToolspec", "shared-spec", "acme", "tenant-a")
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(live).Build()

	bundled := stampedClusterScoped("SpiceboxToolspec", "shared-spec", "acme", "tenant-a")

	adopt, conflict, err := checkResourceOwnership(context.Background(), c, bundled, "acme", "tenant-a", true, false)

	require.NoError(t, err)
	assert.False(t, adopt)
	assert.Nil(t, conflict, "an install re-run from its own namespace converges as before")
}

// An object stamped before the namespace label existed carries no namespace at
// all. Reading that as "not ours" would turn every pre-existing cluster-scoped
// object into a conflict that resolveConflicts refuses outright — a hard break
// on the next re-install of every install that already shipped. It falls back
// to the name-only match it always had, and gets stricter on the first
// re-stamp.
//
// The fallback is not an attacker's route: stripping the label means editing
// the object, and anyone who can do that can already overwrite it directly.
func TestCheckResourceOwnership_ClusterScoped_UnstampedNamespaceFallsBackToNameOnly(t *testing.T) {
	live := stampedClusterScoped("SpiceboxToolspec", "shared-spec", "acme", "")
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(live).Build()

	bundled := stampedClusterScoped("SpiceboxToolspec", "shared-spec", "acme", "tenant-b")

	adopt, conflict, err := checkResourceOwnership(context.Background(), c, bundled, "acme", "tenant-b", true, false)

	require.NoError(t, err)
	assert.False(t, adopt)
	assert.Nil(t, conflict, "an object predating the namespace label keeps the ownership it had")
}

// A NAMESPACED object is already separated by its own namespace in the lookup
// key, so the namespace label adds nothing there and must not start refusing
// installs that legitimately re-run under a different install namespace than
// the object's own (the CR's namespace can be set by the bundle).
func TestCheckResourceOwnership_Namespaced_IgnoresTheInstallNamespaceLabel(t *testing.T) {
	live := &unstructured.Unstructured{}
	live.SetAPIVersion("agentprimitives.authzed.com/v1alpha1")
	live.SetKind("MCPServer")
	live.SetNamespace("apps")
	live.SetName("gh")
	live.SetLabels(map[string]string{
		instance.LabelInstall:          "acme",
		instance.LabelInstallNamespace: "tenant-a",
	})
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(live).Build()

	bundled := &unstructured.Unstructured{}
	bundled.SetAPIVersion("agentprimitives.authzed.com/v1alpha1")
	bundled.SetKind("MCPServer")
	bundled.SetNamespace("apps")
	bundled.SetName("gh")

	adopt, conflict, err := checkResourceOwnership(context.Background(), c, bundled, "acme", "tenant-b", false, false)

	require.NoError(t, err)
	assert.False(t, adopt)
	assert.Nil(t, conflict, "a namespaced object is already scoped by its own namespace")
}
