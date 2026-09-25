// pkg/controllers/workspacevolume/janitor_test.go
//
// Unit tests (fake client) for the stranded-volume janitor: a Released,
// hostPath-backed PersistentVolume of the workspace StorageClass whose
// node-affinity names a node that no longer exists is deleted — it is a pure
// API object whose data died with the node, and the provisioner's own
// reclaim can never run for it (its cleanup helper pod has no node to
// schedule on). Everything else is left strictly alone.
package workspacevolume

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testStorageClass = "ap-workspace-rwx"

// buildPV assembles a workspace-shaped PV. Options mutate the default —
// Released, hostPath-backed, workspace class, affined to nodeName.
func buildPV(name, nodeName string, mutate ...func(*corev1.PersistentVolume)) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: testStorageClass,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/local-path-provisioner/ap-workspace/" + name},
			},
			NodeAffinity: &corev1.VolumeNodeAffinity{
				Required: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn,
							Values: []string{nodeName},
						}},
					}},
				},
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
	for _, m := range mutate {
		m(pv)
	}
	return pv
}

func liveNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func reconcilePV(t *testing.T, pv *corev1.PersistentVolume, extra ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{pv}, extra...)...).Build()
	r := &Reconciler{Client: c, StorageClass: testStorageClass}
	// PVs are cluster-scoped: the request carries a bare name.
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: pv.Name}})
	require.NoError(t, err)
	return c
}

func pvExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var pv corev1.PersistentVolume
	err := c.Get(context.Background(), client.ObjectKey{Name: name}, &pv)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestJanitor_DeadNodeReleasedVolumeIsDeleted(t *testing.T) {
	pv := buildPV("pvc-dead", "node-gone")

	c := reconcilePV(t, pv) // no Node objects: the node does not exist

	assert.False(t, pvExists(t, c, "pvc-dead"),
		"a Released workspace volume on a nonexistent node is unreclaimable garbage and must go")
}

func TestJanitor_EverythingElseIsLeftAlone(t *testing.T) {
	cases := []struct {
		name  string
		pv    *corev1.PersistentVolume
		extra []client.Object
	}{
		{
			name:  "live node: the provisioner's own reclaim runs there, not ours",
			pv:    buildPV("pvc-live", "node-alive"),
			extra: []client.Object{liveNode("node-alive")},
		},
		{
			name: "Bound volume: in use, whatever its node looks like",
			pv: buildPV("pvc-bound", "node-gone", func(pv *corev1.PersistentVolume) {
				pv.Status.Phase = corev1.VolumeBound
			}),
		},
		{
			name: "foreign storage class: not ours to judge",
			pv: buildPV("pvc-foreign", "node-gone", func(pv *corev1.PersistentVolume) {
				pv.Spec.StorageClassName = "some-other-class"
			}),
		},
		{
			name: "non-hostPath source: node death does not destroy its data",
			pv: buildPV("pvc-csi", "node-gone", func(pv *corev1.PersistentVolume) {
				pv.Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{Driver: "pd.csi.storage.gke.io", VolumeHandle: "vh"},
				}
			}),
		},
		{
			name: "no node affinity: cannot prove the data is gone, so keep it",
			pv: buildPV("pvc-noaffinity", "node-gone", func(pv *corev1.PersistentVolume) {
				pv.Spec.NodeAffinity = nil
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := reconcilePV(t, tc.pv, tc.extra...)
			assert.True(t, pvExists(t, c, tc.pv.Name), "volume must be kept")
		})
	}
}
