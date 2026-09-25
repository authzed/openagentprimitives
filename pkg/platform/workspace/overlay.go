package workspace

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// overlayCutDeadlineSeconds bounds a stuck/unschedulable cut so it trips
// JobFailed(DeadlineExceeded) -> the session's WorkspaceOverlayFailed path
// instead of an unbounded silent requeue.
const overlayCutDeadlineSeconds = 900 // 15m

// OverlayCutJobName is the deterministic Job name for cutting a WorkspaceSource
// base into the dst (session workspace) PVC.
func OverlayCutJobName(dst PVCRef) string { return "ws-overlay-" + dst.Name }

// BuildOverlayCutJob constructs the one-shot Job that reflink-copies the
// materialized checkout at <base>/<checkoutSubdir> into the dst PVC's root —
// seeding a per-session copy-on-write overlay of a WorkspaceSource. Mirrors
// BuildSnapshotJob's mount shape: base mounted read-only at /base, dst
// read-write at /dst. cp -a --reflink=auto is O(metadata) on CoW-capable
// filesystems, full copy elsewhere. base and dst must colocate (a single pod
// mounts both) — the operator provisions them from a shared storage class.
func BuildOverlayCutJob(cfg CPByPodConfig, base PVCRef, dst PVCRef, checkoutSubdir string) *batchv1.Job {
	ns := cfg.Namespace
	if dst.Namespace != "" {
		ns = dst.Namespace
	}
	// checkoutSubdir is a controlled constant ("tree"), never user input.
	cmd := fmt.Sprintf("set -e; cp -a --reflink=auto /base/%s/. /dst/", checkoutSubdir)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      OverlayCutJobName(dst),
			Labels: map[string]string{
				"app.kubernetes.io/component": "workspace-overlay-cut",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr.To(int32(1)),
			ActiveDeadlineSeconds: ptr.To(int64(overlayCutDeadlineSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: jobPodMeta("workspace-overlay-cut", nil),
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cfg.ServiceAccount,
					Containers: []corev1.Container{{
						Name:    "cp",
						Image:   cfg.Image,
						Command: []string{"sh"},
						Args:    []string{"-c", cmd},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "base", MountPath: "/base", ReadOnly: true},
							{Name: "dst", MountPath: "/dst"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "base", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: base.Name, ReadOnly: true}}},
						{Name: "dst", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dst.Name}}},
					},
				},
			},
		},
	}
}
