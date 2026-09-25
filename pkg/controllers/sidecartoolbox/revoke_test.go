package sidecartoolbox_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/sidecartoolbox"
)

func TestSidecarToolboxRevokeKeyScope(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-sidecar",
			Namespace: "ops",
		},
	}
	key, scope := sidecartoolbox.SidecarToolboxRevokeKeyScope(cr)
	// Key must match originTool.Origin() = "sidecartoolbox/" + rt.Ref,
	// where rt.Ref == SidecarToolbox CR metadata.Name.
	assert.Equal(t, "sidecartoolbox/my-sidecar", key, "key must match originTool.Origin()")
	assert.Equal(t, "ops", scope, "scope must be the CR namespace")
}

func TestSidecarToolboxFromDelete_DirectObject(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "my-sidecar", Namespace: "ns1"},
	}
	got := sidecartoolbox.SidecarToolboxFromDelete(cr)
	require.NotNil(t, got)
	assert.Equal(t, "my-sidecar", got.Name)
}

func TestSidecarToolboxFromDelete_Tombstone(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "my-sidecar", Namespace: "ns1"},
	}
	tomb := toolscache.DeletedFinalStateUnknown{
		Key: "ns1/my-sidecar",
		Obj: cr,
	}
	got := sidecartoolbox.SidecarToolboxFromDelete(tomb)
	require.NotNil(t, got)
	assert.Equal(t, "my-sidecar", got.Name)
}

func TestSidecarToolboxFromDelete_Garbage(t *testing.T) {
	got := sidecartoolbox.SidecarToolboxFromDelete("not-a-cr")
	assert.Nil(t, got)
}

func TestSidecarToolboxFromDelete_TombstoneWithGarbage(t *testing.T) {
	tomb := toolscache.DeletedFinalStateUnknown{
		Key: "ns1/my-sidecar",
		Obj: "not-a-cr",
	}
	got := sidecartoolbox.SidecarToolboxFromDelete(tomb)
	assert.Nil(t, got)
}
