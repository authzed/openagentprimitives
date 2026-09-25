package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
)

func floorScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, storagev1.AddToScheme(s))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

func storageClass(name, provisioner string, params map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: provisioner,
		Parameters:  params,
	}
}

// The runtime PVC size must be raised to the StorageClass's known minimum, so a
// 2Gi workspace / 8Gi snapshot request never lands below Filestore multishare's
// 10 GiB share floor (the incident: PVC stuck Pending "less than minimum share
// size"). A class with no known floor is left untouched.
func TestEnsureWorkspacePVC_ClampsSizeToClassFloor(t *testing.T) {
	cases := []struct {
		name        string
		provisioner string
		params      map[string]string
		requested   string
		wantSize    string
	}{
		{"filestore multishare raises 2Gi to 10Gi", "filestore.csi.storage.gke.io", map[string]string{"multishare": "true"}, "2Gi", "10Gi"},
		{"hyperdisk raises 2Gi to 4Gi", "pd.csi.storage.gke.io", map[string]string{"type": "hyperdisk-balanced"}, "2Gi", "4Gi"},
		{"local-path leaves 2Gi unchanged (no known floor)", "rancher.io/local-path", nil, "2Gi", "2Gi"},
		{"request already above floor is unchanged", "filestore.csi.storage.gke.io", map[string]string{"multishare": "true"}, "20Gi", "20Gi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := storageClass("ws-class", tc.provisioner, tc.params)
			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "default", UID: types.UID("uid-1")},
			}
			c := fake.NewClientBuilder().WithScheme(floorScheme(t)).WithObjects(sc).Build()
			r := &Reconciler{Client: c, WorkspaceStorageClass: "ws-class", WorkspaceSize: tc.requested}

			name, err := r.ensureWorkspacePVC(context.Background(), sess)
			require.NoError(t, err)
			require.Equal(t, "rb-workspace", name)

			var pvc corev1.PersistentVolumeClaim
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &pvc))
			assert.Equal(t, tc.wantSize, pvc.Spec.Resources.Requests.Storage().String())
		})
	}
}

func TestEnsureSnapshotStorePVC_ClampsSizeToClassFloor(t *testing.T) {
	sc := storageClass("ws-class", "filestore.csi.storage.gke.io", map[string]string{"multishare": "true"})
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "default", UID: types.UID("uid-1")},
	}
	c := fake.NewClientBuilder().WithScheme(floorScheme(t)).WithObjects(sc).Build()
	r := &Reconciler{Client: c, WorkspaceStorageClass: "ws-class", SnapshotStoreSize: "8Gi"}

	name, err := EnsureSnapshotStorePVC(context.Background(), r, sess)
	require.NoError(t, err)
	require.Equal(t, podspec.SnapshotStoreClaimName(sess), name)

	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &pvc))
	assert.Equal(t, "10Gi", pvc.Spec.Resources.Requests.Storage().String())
}
