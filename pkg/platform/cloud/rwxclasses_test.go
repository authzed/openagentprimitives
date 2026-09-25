package cloud

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// filestoreClass builds a Filestore StorageClass with an optional multishare
// parameter, which ListRWXClasses reads to distinguish a shared instance from a
// per-PVC dedicated one.
func filestoreClass(name string, multishare bool) *storagev1.StorageClass {
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: "filestore.csi.storage.gke.io",
	}
	if multishare {
		sc.Parameters = map[string]string{"multishare": "true"}
	}
	return sc
}

// ListRWXClasses returns exactly the RWX-capable classes — every known-RWX
// provisioner plus the bundled local-path class — and excludes block-storage
// classes (pd.csi, gce-pd) that cannot back a shared workspace. Each is
// annotated so the picker can label it (bundled = node-pinned, Filestore =
// cross-node, multishare = shared instance).
func TestListRWXClasses(t *testing.T) {
	objs := []runtime.Object{
		storageClassObj("ap-workspace-rwx", "cluster.local/ap-workspace-provisioner"),
		filestoreClass("enterprise-multishare-rwx", true),
		filestoreClass("enterprise-rwx", false),
		storageClassObj("nfs-sc", "nfs.csi.k8s.io"),
		// Excluded: block storage cannot be ReadWriteMany.
		storageClassObj("hyperdisk-balanced", "pd.csi.storage.gke.io"),
		storageClassObj("standard", "kubernetes.io/gce-pd"),
	}
	kc := fake.NewSimpleClientset(objs...)

	got, err := ListRWXClasses(context.Background(), kc)
	require.NoError(t, err)

	byName := map[string]RWXClassInfo{}
	for _, c := range got {
		byName[c.Name] = c
	}

	// Excluded classes never appear.
	assert.NotContains(t, byName, "hyperdisk-balanced")
	assert.NotContains(t, byName, "standard")
	assert.Len(t, got, 4, "exactly the 4 RWX-capable classes")

	assert.Equal(t, RWXClassInfo{Name: "ap-workspace-rwx", Provisioner: "cluster.local/ap-workspace-provisioner", Bundled: true}, byName["ap-workspace-rwx"])
	assert.Equal(t, RWXClassInfo{Name: "enterprise-multishare-rwx", Provisioner: "filestore.csi.storage.gke.io", Filestore: true, Multishare: true}, byName["enterprise-multishare-rwx"])
	assert.Equal(t, RWXClassInfo{Name: "enterprise-rwx", Provisioner: "filestore.csi.storage.gke.io", Filestore: true}, byName["enterprise-rwx"])
	assert.Equal(t, RWXClassInfo{Name: "nfs-sc", Provisioner: "nfs.csi.k8s.io"}, byName["nfs-sc"])
}
