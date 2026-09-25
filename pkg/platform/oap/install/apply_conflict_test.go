package install

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// foreignUnstructured builds a live pre-existing object with no
// instance.LabelInstall — the shape checkResourceOwnership must report as a
// conflict rather than "ours" or "absent".
func foreignUnstructured(apiVersion, kind, ns, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetNamespace(ns)
	obj.SetName(name)
	return obj
}

// TestGuardSynthesizedSecrets_ForeignSecret_ReportsSecretTrue pins that a
// synthesized-Secret conflict comes back with Secret: true even though
// guardSynthesizedSecrets never sets the field: checkResourceOwnership derives
// it from obj.GetKind() at the single Conflict construction site.
func TestGuardSynthesizedSecrets_ForeignSecret_ReportsSecretTrue(t *testing.T) {
	const ns = "test-ns"
	foreign := foreignUnstructured("v1", "Secret", ns, "app-secret")

	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(foreign).Build()

	sec, err := secretUnstructured(SecretSpec{Name: "app-secret", Key: "token", Value: "bundle-value"}, ns)
	require.NoError(t, err)

	conflicts, err := guardSynthesizedSecrets(context.Background(), c, []*unstructured.Unstructured{sec}, "some-install", ns)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)
	assert.True(t, conflicts[0].Secret, "a synthesized Secret conflict must still report Secret: true, derived from the object's own Kind rather than a caller-side assignment")
}

// TestCheckResourceOwnership_NonSecretConflict_ReportsSecretFalse is the other
// half: a bundled CR that happens not to be a Secret must report Secret: false
// from the SAME derivation checkResourceOwnership uses for a Secret, proving the
// field tracks obj.GetKind() rather than "which guard called me".
// planBundledResources never marks anything Secret today only because no bundled
// CR is ever a Secret Kind; this exercises the derivation directly so that holds
// even if that changes.
func TestCheckResourceOwnership_NonSecretConflict_ReportsSecretFalse(t *testing.T) {
	const ns = "test-ns"
	foreign := foreignUnstructured("v1", "ConfigMap", ns, "shared-config")

	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(foreign).Build()

	bundled := foreignUnstructured("v1", "ConfigMap", ns, "shared-config")

	adopt, conflict, err := checkResourceOwnership(context.Background(), c, bundled, "some-install", ns, false, false)
	require.NoError(t, err)
	assert.False(t, adopt)
	require.NotNil(t, conflict)
	assert.False(t, conflict.Secret, "a non-Secret bundled CR conflict must report Secret: false")
}
