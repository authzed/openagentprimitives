package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
)

func TestEnsureSnapshotStorePVC_CreatesWhenMissing(t *testing.T) {
	ctx := context.Background()
	sch := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(sch))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	c := fake.NewClientBuilder().WithScheme(sch).Build()

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns", UID: "uid-1"},
	}
	require.NoError(t, c.Create(ctx, sess))

	r := &Reconciler{Client: c, WorkspaceStorageClass: "ap-workspace-rwx", WorkspaceSize: "2Gi", SnapshotStoreSize: "8Gi"}
	name, err := EnsureSnapshotStorePVC(ctx, r, sess)
	require.NoError(t, err)
	assert.Equal(t, podspec.SnapshotStoreClaimName(sess), name)

	var got corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: name}, &got))
	assert.Equal(t, "ap-workspace-rwx", *got.Spec.StorageClassName)
}

// With SnapshotStoreSize unset, the snapshot-store PVC follows the workspace
// size (not a fixed oversized 8Gi) — so on a node-pinned class a session's
// node-ephemeral footprint is workspace+workspace, not workspace+8Gi. A class
// with no floor (local-path) leaves it at the workspace size.
func TestEnsureSnapshotStorePVC_DefaultsToWorkspaceSize(t *testing.T) {
	ctx := context.Background()
	sc := storageClass("ap-workspace-rwx", "cluster.local/ap-workspace-provisioner", nil)
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns", UID: "uid-1"}}
	c := fake.NewClientBuilder().WithScheme(floorScheme(t)).WithObjects(sc, sess).Build()
	r := &Reconciler{Client: c, WorkspaceStorageClass: "ap-workspace-rwx", WorkspaceSize: "2Gi"} // SnapshotStoreSize deliberately unset

	name, err := EnsureSnapshotStorePVC(ctx, r, sess)
	require.NoError(t, err)
	var got corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: name}, &got))
	assert.Equal(t, "2Gi", got.Spec.Resources.Requests.Storage().String(), "unset snapshot size must follow the 2Gi workspace, not default to 8Gi")
}

func TestEnsureSnapshotStorePVC_NoStorageClass_Skips(t *testing.T) {
	ctx := context.Background()
	sch := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(sch))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	c := fake.NewClientBuilder().WithScheme(sch).Build()

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns", UID: "uid-1"},
	}
	require.NoError(t, c.Create(ctx, sess))

	r := &Reconciler{Client: c, WorkspaceStorageClass: "", SnapshotStoreSize: "8Gi"}
	name, err := EnsureSnapshotStorePVC(ctx, r, sess)
	require.NoError(t, err)
	assert.Empty(t, name, "no storage class → no snapshot store; restart will be gated")

	var got corev1.PersistentVolumeClaim
	err = c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: podspec.SnapshotStoreClaimName(sess)}, &got)
	assert.True(t, errors.IsNotFound(err))
}

func TestEnsureSnapshotStorePVC_Idempotent(t *testing.T) {
	ctx := context.Background()
	sch := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(sch))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	c := fake.NewClientBuilder().WithScheme(sch).Build()

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns", UID: "uid-1"},
	}
	require.NoError(t, c.Create(ctx, sess))
	r := &Reconciler{Client: c, WorkspaceStorageClass: "ap-workspace-rwx", SnapshotStoreSize: "8Gi"}

	for i := 0; i < 3; i++ {
		_, err := EnsureSnapshotStorePVC(ctx, r, sess)
		require.NoError(t, err)
	}
	var list corev1.PersistentVolumeClaimList
	require.NoError(t, c.List(ctx, &list))
	snapstores := 0
	for _, pvc := range list.Items {
		if pvc.Name == podspec.SnapshotStoreClaimName(sess) {
			snapstores++
		}
	}
	assert.Equal(t, 1, snapstores, "ensure should be idempotent")
}
