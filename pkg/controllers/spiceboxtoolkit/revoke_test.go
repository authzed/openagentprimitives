package spiceboxtoolkit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxtoolkit"
)

func TestSpiceboxToolkitRevokeKeyScope(t *testing.T) {
	// CR metadata.Name and Spec.Name intentionally differ to confirm we use
	// Spec.Name (the value that matches SandboxTool.Origin()).
	cr := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{
			// Cluster-scoped: no namespace.
			Name: "my-tool-v1",
		},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name: "my-tool",
		},
	}
	key, scope := spiceboxtoolkit.SpiceboxToolkitRevokeKeyScope(cr)
	// Key must match SandboxTool.Origin() = "toolkit/" + toolkit.Toolkit.Name,
	// where Toolkit.Name == SpiceboxToolkitSpec.Name (NOT CR metadata.Name).
	assert.Equal(t, "toolkit/my-tool", key, "key must match SandboxTool.Origin() using Spec.Name")
	assert.Equal(t, "", scope, "scope must be empty: SpiceboxToolkit is cluster-scoped")
}

func TestSpiceboxToolkitRevokeKeyScope_EmptySpecName(t *testing.T) {
	// A CR with no Spec.Name must produce "toolkit/" as key.  The DeleteFunc
	// guards against this before calling RevokePublisher.Emit so that a
	// meaningless "toolkit/" key is never published.  This test pins the raw
	// helper output so the guard condition stays in sync: callers must check
	// Spec.Name != "" before invoking SpiceboxToolkitRevokeKeyScope.
	cr := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "no-spec-name"},
		Spec:       spiceboxv1alpha1.SpiceboxToolkitSpec{},
	}
	key, scope := spiceboxtoolkit.SpiceboxToolkitRevokeKeyScope(cr)
	assert.Equal(t, "toolkit/", key, "empty Spec.Name yields a meaningless key; callers must guard")
	assert.Equal(t, "", scope)
}

func TestSpiceboxToolkitFromDelete_DirectObject(t *testing.T) {
	cr := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "my-tool-v1"},
		Spec:       spiceboxv1alpha1.SpiceboxToolkitSpec{Name: "my-tool"},
	}
	got := spiceboxtoolkit.SpiceboxToolkitFromDelete(cr)
	require.NotNil(t, got)
	assert.Equal(t, "my-tool-v1", got.Name)
	assert.Equal(t, "my-tool", got.Spec.Name)
}

func TestSpiceboxToolkitFromDelete_Tombstone(t *testing.T) {
	cr := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "my-tool-v1"},
		Spec:       spiceboxv1alpha1.SpiceboxToolkitSpec{Name: "my-tool"},
	}
	tomb := toolscache.DeletedFinalStateUnknown{
		Key: "my-tool-v1",
		Obj: cr,
	}
	got := spiceboxtoolkit.SpiceboxToolkitFromDelete(tomb)
	require.NotNil(t, got)
	assert.Equal(t, "my-tool-v1", got.Name)
}

func TestSpiceboxToolkitFromDelete_Garbage(t *testing.T) {
	got := spiceboxtoolkit.SpiceboxToolkitFromDelete("not-a-cr")
	assert.Nil(t, got)
}

func TestSpiceboxToolkitFromDelete_TombstoneWithGarbage(t *testing.T) {
	tomb := toolscache.DeletedFinalStateUnknown{
		Key: "my-tool-v1",
		Obj: "not-a-cr",
	}
	got := spiceboxtoolkit.SpiceboxToolkitFromDelete(tomb)
	assert.Nil(t, got)
}
