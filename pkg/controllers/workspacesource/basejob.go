package workspacesource

import (
	"fmt"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
)

const (
	baseMountPath = "/base"

	// BaseCheckoutSubdir is the subdirectory within the base PVC that holds the
	// materialized checkout (MaterializeCommands clone into baseCheckoutDir =
	// baseMountPath + "/" + BaseCheckoutSubdir). The per-session overlay cut (a
	// later phase / the agentsession controller) reflinks this subtree, not the
	// PVC root, into the session workspace.
	BaseCheckoutSubdir = "tree"

	baseCheckoutDir = baseMountPath + "/" + BaseCheckoutSubdir

	// BaseGenerationLabel stamps the materialize Job with the WorkspaceSource
	// generation it was built from. The Job is created once by stable name and
	// never rebuilt in Phase 2, so this label is how the controller detects
	// that the spec has since moved on and the base no longer reflects it.
	BaseGenerationLabel = "agentprimitives.authzed.com/workspacesource-generation"
)

// jobDeadlineSeconds bounds a stuck materialize or refresh Job so it trips
// JobFailed(DeadlineExceeded) instead of hanging forever — the materialize
// path surfaces that as Ready=False/MaterializeFailed; the refresh path
// treats it as a failed refresh attempt (base stays usable, retried next
// interval).
const jobDeadlineSeconds = 900 // 15m

// generationString renders a WorkspaceSource generation for use as a label
// value.
func generationString(g int64) string { return strconv.FormatInt(g, 10) }

// BaseClaimName is the deterministic base-PVC name for a WorkspaceSource.
func BaseClaimName(ws *spiceboxv1alpha1.WorkspaceSource) string { return "ws-base-" + ws.Name }

// MaterializeJobName is the deterministic materialize-Job name.
func MaterializeJobName(ws *spiceboxv1alpha1.WorkspaceSource) string { return "ws-mat-" + ws.Name }

// RefreshJobName is the deterministic refresh-Job name. Distinct from
// MaterializeJobName so a scheduled refresh never collides with (or is
// mistaken for) the one-time materialize Job.
func RefreshJobName(ws *spiceboxv1alpha1.WorkspaceSource) string { return "ws-refresh-" + ws.Name }

func ownerRef(ws *spiceboxv1alpha1.WorkspaceSource) []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion:         spiceboxv1alpha1.SchemeGroupVersion.String(),
		Kind:               "WorkspaceSource",
		Name:               ws.Name,
		UID:                ws.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
}

// BuildBasePVC constructs the shared read-cache base PVC (RWO portable floor,
// owner-referenced so K8s GC removes it with the WorkspaceSource). Mirrors
// agentsession.BuildWorkspacePVC.
//
// DEFERRED (multi-node): the base is RWO, and the per-session overlay-cut Job
// mounts BOTH this base and the session's overlay PVC. On a multi-node cluster
// where the two PVCs bind to different nodes, the cut Job cannot schedule (RWO
// can't cross nodes). Making the base RWX (or adding topology-aware
// co-scheduling of the overlay onto the base's node) is the fix; until then
// the shared-base fast path is single-node-effective. Configure an RWX base
// StorageClass to lift this. Tracked as a known deferral in the feature design.
func BuildBasePVC(ws *spiceboxv1alpha1.WorkspaceSource, storageClass, size string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            BaseClaimName(ws),
			Namespace:       ws.Namespace,
			Labels:          map[string]string{"app.kubernetes.io/component": "workspace-base", "agentprimitives.authzed.com/workspacesource": ws.Name},
			OwnerReferences: ownerRef(ws),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptr.To(storageClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
}

func corev1EnvVar(k, v string) corev1.EnvVar { return corev1.EnvVar{Name: k, Value: v} }

func toEnvVars(env []string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			out = append(out, corev1EnvVar(k, v))
		}
	}
	return out
}

// buildBaseJob is the shared Job/PVC-mount shape behind BuildMaterializeJob
// and BuildRefreshJob: both run a driver's commands as sequential
// initContainers (argv, never shell — preserving Phase 1's flag-injection
// safety) against the base PVC mounted at /base, with a trivial main
// container so the Job completes. They differ only in name, labels, and the
// initContainer name prefix. Mirrors workspace.BuildSnapshotJob's shape.
func buildBaseJob(ws *spiceboxv1alpha1.WorkspaceSource, name string, labels map[string]string, initNamePrefix, baseClaim, serviceAccount, image string, cmds []workspacekinds.Command) *batchv1.Job {
	mount := corev1.VolumeMount{Name: "base", MountPath: baseMountPath}
	inits := make([]corev1.Container, 0, len(cmds))
	for i, c := range cmds {
		if len(c.Argv) == 0 {
			continue
		}
		inits = append(inits, corev1.Container{
			Name:         fmt.Sprintf("%s-%d", initNamePrefix, i),
			Image:        image,
			Command:      []string{c.Argv[0]},
			Args:         c.Argv[1:],
			Env:          toEnvVars(c.Env),
			WorkingDir:   c.Dir,
			VolumeMounts: []corev1.VolumeMount{mount},
		})
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       ws.Namespace,
			Labels:          labels,
			OwnerReferences: ownerRef(ws),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr.To(int32(1)),
			ActiveDeadlineSeconds: ptr.To(int64(jobDeadlineSeconds)),
			Template: corev1.PodTemplateSpec{
				// The Job's own labels select nothing — a NetworkPolicy selects
				// PODS. Without them this pod matches no policy, and
				// NetworkPolicy's allow-union leaves an unmatched pod
				// unrestricted in both directions on an enforcing CNI. This one
				// runs git against a remote with an injected credential.
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: serviceAccount,
					InitContainers:     inits,
					Containers: []corev1.Container{{
						Name: "done", Image: image, Command: []string{"true"},
						VolumeMounts: []corev1.VolumeMount{mount},
					}},
					Volumes: []corev1.Volume{{
						Name: "base",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: baseClaim},
						},
					}},
				},
			},
		},
	}
}

// BuildMaterializeJob builds the one-time base-checkout Job, stamped with the
// spec generation it was built from (see BaseGenerationLabel).
func BuildMaterializeJob(ws *spiceboxv1alpha1.WorkspaceSource, baseClaim, serviceAccount, image string, cmds []workspacekinds.Command) *batchv1.Job {
	return buildBaseJob(ws, MaterializeJobName(ws), map[string]string{
		"app.kubernetes.io/component":                 "workspace-materialize",
		"agentprimitives.authzed.com/workspacesource": ws.Name,
		BaseGenerationLabel:                           generationString(ws.Generation),
	}, "materialize", baseClaim, serviceAccount, image, cmds)
}

// BuildRefreshJob builds the scheduled base-refresh Job (spec.base.refresh):
// it runs the driver's SyncCommands (a pull, not a re-clone) against the same
// base PVC mounted at /base. Unlike BuildMaterializeJob it carries no
// generation label — a refresh re-pulls the current origin state regardless
// of spec generation, it doesn't re-materialize a specific one.
func BuildRefreshJob(ws *spiceboxv1alpha1.WorkspaceSource, baseClaim, serviceAccount, image string, cmds []workspacekinds.Command) *batchv1.Job {
	return buildBaseJob(ws, RefreshJobName(ws), map[string]string{
		"app.kubernetes.io/component":                 "workspace-refresh",
		"agentprimitives.authzed.com/workspacesource": ws.Name,
	}, "refresh", baseClaim, serviceAccount, image, cmds)
}
