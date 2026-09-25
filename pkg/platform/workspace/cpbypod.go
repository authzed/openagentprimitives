package workspace

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CPByPodConfig configures the cp-by-pod Snapshotter implementation.
type CPByPodConfig struct {
	// Namespace is where the snapshot/restore Jobs run. Typically
	// the AgentSession's namespace.
	Namespace string

	// SnapshotStorePVC is the RWX PVC where snapshot contents are
	// accumulated. One per AgentSession, ensured by the AgentSession
	// controller.
	SnapshotStorePVC string

	// Image is the container image whose entrypoint can run sh -c.
	// busybox is the default; can be any image with `cp` + `sh`.
	Image string

	// ServiceAccount the Jobs run under. Should have no extra K8s
	// perms beyond default (pod, secret-mount-via-projection). The
	// operator provisions this SA at install time.
	ServiceAccount string
}

// snapshotJobTTLSeconds auto-deletes a finished snapshot/restore/gc Job (and its
// pod) this long after it completes. Without it, completed Job pods linger
// indefinitely, and because each references the workspace + snapshot-store PVCs,
// the kubernetes.io/pvc-protection finalizer treats those PVCs as in-use — so a
// terminal session's storage reclaim deletes the PVCs but they never leave
// Terminating and the node-local disk is never freed. Observed in production:
// 339 snapshot Jobs accumulated, pinning reclaimed PVCs and driving nodes into
// ephemeral-storage pressure. 10 min is well past when WaitPreDispatchSnapshot
// observes completion and records the audit, and comfortably inside the
// node-pinned reclaim grace, so the Jobs are gone before reclaim runs.
const snapshotJobTTLSeconds = 600

// BuildSnapshotJob constructs the one-shot Job that does the cp from
// src into snapshot-store at the handle's path segment.
// `cp -a --reflink=auto` is the magic: O(metadata) on CoW-capable
// filesystems; falls back to full copy elsewhere.
//
// Namespace resolution (highest priority first):
//  1. src.Namespace — the source PVC's namespace; used in operator
//     mode where one Snapshotter serves many namespaces.
//  2. cfg.Namespace — the static override (test fixtures, single-ns setups).
//
// SnapshotStorePVC resolution: h.SnapshotStorePVC if non-empty,
// otherwise cfg.SnapshotStorePVC. This lets the operator pass a
// per-AgentSession store PVC via the handle without touching cfg.
func BuildSnapshotJob(cfg CPByPodConfig, src PVCRef, h SnapshotHandle) *batchv1.Job {
	ns := cfg.Namespace
	if src.Namespace != "" {
		ns = src.Namespace
	}
	storePVC := cfg.SnapshotStorePVC
	if h.SnapshotStorePVC != "" {
		storePVC = h.SnapshotStorePVC
	}
	cmd := fmt.Sprintf(
		"set -e; mkdir -p /snap/%s; cp -a --reflink=auto /src/. /snap/%s/",
		h.PathSegment(), h.PathSegment(),
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      jobNameSnapshot(h),
			Labels: map[string]string{
				"app.kubernetes.io/component":            "workspace-snapshot",
				"agentprimitives.authzed.com/sessionUID": h.SessionUID,
				"agentprimitives.authzed.com/turnIndex":  labelTurnIndex(h.TurnIndex),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(1)),
			TTLSecondsAfterFinished: ptr.To(int32(snapshotJobTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: jobPodMeta("workspace-snapshot", nil),
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cfg.ServiceAccount,
					Containers: []corev1.Container{{
						Name:    "cp",
						Image:   cfg.Image,
						Command: []string{"sh"},
						Args:    []string{"-c", cmd},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "src", MountPath: "/src", ReadOnly: true},
							{Name: "snap", MountPath: "/snap"},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "src",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: src.Name, ReadOnly: true,
								},
							},
						},
						{
							Name: "snap",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: storePVC,
								},
							},
						},
					},
				},
			},
		},
	}
}

func jobNameSnapshot(h SnapshotHandle) string {
	return fmt.Sprintf("snap-%s-%s", h.SessionUID, h.turnSegment())
}

func jobNameRestore(h SnapshotHandle, dst PVCRef) string {
	return fmt.Sprintf("rest-%s-%s-%s", h.SessionUID, h.turnSegment(), dst.Name)
}

// SnapshotJobName is the canonical, deterministic name of the snapshot
// Job for handle h — the single source of truth shared by
// BuildSnapshotJob, CPByPod.SnapshotDone, and external consumers (the
// toolcall controller's WaitPreDispatchSnapshot, the e2e
// completingSnapshotter). Do not re-derive the format elsewhere.
func SnapshotJobName(h SnapshotHandle) string {
	return jobNameSnapshot(h)
}

// labelTurnIndex renders the turnIndex label value; negative (ad-hoc)
// indexes render as "adhoc" — "-1" is not a valid label value.
func labelTurnIndex(turnIndex int) string {
	if turnIndex < 0 {
		return "adhoc"
	}
	return fmt.Sprintf("%d", turnIndex)
}

// BuildRestoreJob constructs the one-shot Job that copies the
// snapshot at handle h into the dst PVC. Source mount is the
// snapshot-store PVC at /snap (read-only); dst is mounted RW at /dst.
//
// Namespace resolution: dst.Namespace wins over cfg.Namespace (same
// rule as BuildSnapshotJob). SnapshotStorePVC resolution: h.SnapshotStorePVC
// if non-empty, otherwise cfg.SnapshotStorePVC.
func BuildRestoreJob(cfg CPByPodConfig, h SnapshotHandle, dst PVCRef) *batchv1.Job {
	ns := cfg.Namespace
	if dst.Namespace != "" {
		ns = dst.Namespace
	}
	storePVC := cfg.SnapshotStorePVC
	if h.SnapshotStorePVC != "" {
		storePVC = h.SnapshotStorePVC
	}
	cmd := fmt.Sprintf("set -e; cp -a --reflink=auto /snap/%s/. /dst/", h.PathSegment())
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      jobNameRestore(h, dst),
			Labels: map[string]string{
				"app.kubernetes.io/component":            "workspace-restore",
				"agentprimitives.authzed.com/sessionUID": h.SessionUID,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(1)),
			TTLSecondsAfterFinished: ptr.To(int32(snapshotJobTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: jobPodMeta("workspace-restore", nil),
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cfg.ServiceAccount,
					Containers: []corev1.Container{{
						Name:    "cp",
						Image:   cfg.Image,
						Command: []string{"sh"},
						Args:    []string{"-c", cmd},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "snap", MountPath: "/snap", ReadOnly: true},
							{Name: "dst", MountPath: "/dst"},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "snap",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: storePVC, ReadOnly: true,
								},
							},
						},
						{
							Name: "dst",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: dst.Name,
								},
							},
						},
					},
				},
			},
		},
	}
}

// CPByPod is the default Snapshotter implementation. It launches
// one-shot Jobs that mount source + snapshot-store (or
// snapshot-store + dst) and run cp -a --reflink=auto.
type CPByPod struct {
	k8s client.Client
	cfg CPByPodConfig
}

// NewCPByPod constructs a CPByPod. k8s is the controller-runtime
// client used to create/get/wait-on Jobs.
func NewCPByPod(k8s client.Client, cfg CPByPodConfig) *CPByPod {
	return &CPByPod{k8s: k8s, cfg: cfg}
}

// Snapshot creates the snapshot Job. Idempotent: a second call at
// the same handle no-ops via Create's AlreadyExists handling. The
// method returns when the Job has been created — callers (the toolcall
// controller in B8) wait separately on the Job's completion.
func (s *CPByPod) Snapshot(ctx context.Context, src PVCRef, h SnapshotHandle) error {
	job := BuildSnapshotJob(s.cfg, src, h)
	if err := s.k8s.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("CPByPod.Snapshot: create Job: %w", err)
	}
	return nil
}

// Restore creates the restore Job. Same idempotency contract as
// Snapshot.
func (s *CPByPod) Restore(ctx context.Context, h SnapshotHandle, dst PVCRef) error {
	job := BuildRestoreJob(s.cfg, h, dst)
	if err := s.k8s.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("CPByPod.Restore: create Job: %w", err)
	}
	return nil
}

// SnapshotDone reports whether the snapshot Job for h has finished.
// Namespace resolution matches BuildSnapshotJob (src.Namespace wins
// over cfg.Namespace) so the probe looks up exactly the Job Snapshot
// created.
func (s *CPByPod) SnapshotDone(ctx context.Context, src PVCRef, h SnapshotHandle) (bool, error) {
	ns := s.cfg.Namespace
	if src.Namespace != "" {
		ns = src.Namespace
	}
	return s.jobDone(ctx, ns, jobNameSnapshot(h))
}

// RestoreDone reports whether the restore Job for (h, dst) has
// finished. Namespace resolution matches BuildRestoreJob
// (dst.Namespace wins over cfg.Namespace).
func (s *CPByPod) RestoreDone(ctx context.Context, h SnapshotHandle, dst PVCRef) (bool, error) {
	ns := s.cfg.Namespace
	if dst.Namespace != "" {
		ns = dst.Namespace
	}
	return s.jobDone(ctx, ns, jobNameRestore(h, dst))
}

// jobDone inspects a snapshot/restore Job's terminal conditions:
// JobComplete=True → (true, nil); JobFailed=True → (false, wrapped
// ErrSnapshotJobFailed carrying the condition's message); no terminal
// condition yet → (false, nil); missing Job → (false, wrapped
// ErrSnapshotNotFound).
func (s *CPByPod) jobDone(ctx context.Context, ns, name string) (bool, error) {
	var job batchv1.Job
	if err := s.k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Errorf("CPByPod: Job %s/%s: %w", ns, name, ErrSnapshotNotFound)
		}
		return false, fmt.Errorf("CPByPod: get Job %s/%s: %w", ns, name, err)
	}
	for _, cond := range job.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			return true, nil
		case batchv1.JobFailed:
			return false, fmt.Errorf("CPByPod: Job %s/%s failed: %s: %w", ns, name, cond.Message, ErrSnapshotJobFailed)
		}
	}
	return false, nil
}

// GC deletes the snapshot subdirectory on the snapshot-store PVC by launching a
// one-shot rm -rf Job, rather than mounting it in-process.
//
// cfg.Namespace must be non-empty: GC is called by controllers that know the
// namespace for the GC Job. Uses h.SnapshotStorePVC if set, else
// cfg.SnapshotStorePVC.
func (s *CPByPod) GC(ctx context.Context, h SnapshotHandle) error {
	if s.cfg.Namespace == "" {
		return fmt.Errorf("CPByPod.GC: cfg.Namespace must be set; configure CPByPod per-namespace for GC")
	}
	storePVC := s.cfg.SnapshotStorePVC
	if h.SnapshotStorePVC != "" {
		storePVC = h.SnapshotStorePVC
	}
	cmd := fmt.Sprintf("rm -rf /snap/%s", h.PathSegment())
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: s.cfg.Namespace,
			Name:      "gc-" + h.SessionUID + "-" + h.turnSegment(),
			Labels: map[string]string{
				"app.kubernetes.io/component": "workspace-snapshot-gc",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(1)),
			TTLSecondsAfterFinished: ptr.To(int32(snapshotJobTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: jobPodMeta("workspace-snapshot-gc", nil),
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: s.cfg.ServiceAccount,
					Containers: []corev1.Container{{
						Name:    "gc",
						Image:   s.cfg.Image,
						Command: []string{"sh"},
						Args:    []string{"-c", cmd},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "snap", MountPath: "/snap"},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "snap",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: storePVC,
							},
						},
					}},
				},
			},
		},
	}
	if err := s.k8s.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("CPByPod.GC: create Job: %w", err)
	}
	return nil
}

// Interface satisfaction (compile-time check).
var _ Snapshotter = (*CPByPod)(nil)

// SnapshotServiceAccount is the ServiceAccount snapshot Jobs run as.
//
// The Job needs no API permissions — it runs cp/rm against mounted PVCs and
// never talks to the apiserver. The account exists only to give tighter
// installs a name to bind pod-security and NetworkPolicy against.
//
// It must exist in the namespace the JOB runs in, which is the SESSION's
// namespace, not the operator's. Installing it only into the system namespace
// left every session elsewhere unable to snapshot at all: the Job cannot create
// a pod, and a Job with no pod is indistinguishable from a slow one.
const SnapshotServiceAccount = "ap-snapshotter"

// EnsureSnapshotServiceAccount creates the SnapshotServiceAccount in ns if it
// is absent. Every caller that launches a Snapshot/Restore/GC Job in a
// namespace other than the operator's must call this first — the install
// bundle creates the account only in the operator's namespace, and a Job
// whose pod cannot be created for want of a ServiceAccount is
// indistinguishable from one still copying.
//
// AlreadyExists is success: concurrent callers in the same namespace race
// here, and the first to win has done the work.
func EnsureSnapshotServiceAccount(ctx context.Context, c client.Client, ns string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SnapshotServiceAccount,
			Namespace: ns,
			Labels:    map[string]string{"app.kubernetes.io/component": "workspace-snapshot"},
		},
	}
	if err := c.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensure ServiceAccount %s/%s: %w", ns, SnapshotServiceAccount, err)
	}
	return nil
}
