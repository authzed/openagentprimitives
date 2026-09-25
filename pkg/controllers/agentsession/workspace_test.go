package agentsession

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestBuildWorkspacePVC(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "default", UID: "uid-1"},
	}
	pvc := BuildWorkspacePVC(sess, "rwx-class", "2Gi")

	assert.Equal(t, "rb-workspace", pvc.Name)
	assert.Equal(t, "default", pvc.Namespace)
	require.Len(t, pvc.OwnerReferences, 1, "PVC must be owned by the AgentSession for GC")
	assert.Equal(t, "rb", pvc.OwnerReferences[0].Name)
	assert.Equal(t, types.UID("uid-1"), pvc.OwnerReferences[0].UID)
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, pvc.Spec.AccessModes)
	require.NotNil(t, pvc.Spec.StorageClassName)
	assert.Equal(t, "rwx-class", *pvc.Spec.StorageClassName)
	assert.Equal(t, "2Gi", pvc.Spec.Resources.Requests.Storage().String())
}
