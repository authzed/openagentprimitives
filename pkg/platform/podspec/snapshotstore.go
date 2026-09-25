package podspec

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// SnapshotStoreClaimName is the deterministic name of the
// snapshot-store PVC paired with an AgentSession's workspace PVC.
// The snapshot store accumulates one subdirectory per JIT snapshot
// taken by the cp-by-pod Snapshotter.
func SnapshotStoreClaimName(sess *spiceboxv1alpha1.AgentSession) string {
	return "ap-snapstore-" + sess.Name
}

// BuildSnapshotStorePVC constructs the PVC the cp-by-pod
// Snapshotter writes snapshot subdirectories into. Owner-ref'd to
// the AgentSession so K8s GC deletes it when the session is deleted
// (same lifecycle as the workspace PVC).
//
// ReadWriteOnce, matching the workspace PVC: under the bundled nodePathMap
// provisioner each PV carries nodeAffinity, and the snapshot/restore copy jobs
// mount workspace + snapstore in one pod — the shared nodeAffinity co-locates
// that pod and both PVCs on one node, where RWO permits the concurrent mounts.
func BuildSnapshotStorePVC(sess *spiceboxv1alpha1.AgentSession, storageClass, size string) *corev1.PersistentVolumeClaim {
	tval := true
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SnapshotStoreClaimName(sess),
			Namespace: sess.Namespace,
			Labels: map[string]string{
				"agentprimitives.authzed.com/agentsession": sess.Name,
				"app.kubernetes.io/component":              "snapshot-store",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:               "AgentSession",
				Name:               sess.Name,
				UID:                sess.UID,
				Controller:         &tval,
				BlockOwnerDeletion: &tval,
			}},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptr.To(storageClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}
}
