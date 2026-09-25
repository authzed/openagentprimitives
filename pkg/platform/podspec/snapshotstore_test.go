package podspec_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
)

func TestSnapshotStoreClaimName_DerivedFromSession(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns"},
	}
	assert.Equal(t, "ap-snapstore-demo", podspec.SnapshotStoreClaimName(sess))
}

func TestBuildSnapshotStorePVC_ShapeAndOwnerRef(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns", UID: "uid-1"},
	}
	pvc := podspec.BuildSnapshotStorePVC(sess, "ap-workspace-rwx", "8Gi")
	require.NotNil(t, pvc)
	assert.Equal(t, "ap-snapstore-demo", pvc.Name)
	assert.Equal(t, "ns", pvc.Namespace)
	assert.Equal(t, "demo", pvc.Labels["agentprimitives.authzed.com/agentsession"])
	require.Len(t, pvc.OwnerReferences, 1)
	assert.Equal(t, "AgentSession", pvc.OwnerReferences[0].Kind)
	assert.Equal(t, sess.UID, pvc.OwnerReferences[0].UID)
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, pvc.Spec.AccessModes)
	require.NotNil(t, pvc.Spec.StorageClassName)
	assert.Equal(t, "ap-workspace-rwx", *pvc.Spec.StorageClassName)
	assert.Equal(t, resource.MustParse("8Gi"), pvc.Spec.Resources.Requests[corev1.ResourceStorage])
}
