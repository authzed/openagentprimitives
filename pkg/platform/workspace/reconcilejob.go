package workspace

import (
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
)

const reconcileMountPath = "/workspace"

// BuildReconcileJob runs a workspace-source driver's sync/apply commands (argv,
// never shell) against a session's overlay PVC mounted at /workspace.
//
// When credEnvVar and credSecret are both non-empty, each command container
// gets credEnvVar sourced from credSecret's credKey via secretKeyRef — Optional,
// so a missing Secret/key does not block the pod and the driver's push instead
// fails auth, surfacing as a failed reconcile rather than a stuck one. This is
// how the per-session write-back credential reaches the driver's commands (e.g.
// git's inline credential.helper reading WORKSPACE_GIT_TOKEN); sync passes
// credEnvVar == "" so nothing is injected. deadlineSeconds bounds a stuck
// reconcile so it fails loudly.
//
// The overlay PVC is RWO and already held by the running session pod. The
// scheduler co-schedules this Job's pod onto the PVC's node via the bound PV's
// node-affinity (the bundled node-pinning provisioner), so same-node RWO
// sharing works. If a cross-node RWX storage class prevents scheduling, the pod
// sits Pending until ActiveDeadlineSeconds trips, surfaced as a failed
// reconcile.
func BuildReconcileJob(cfg CPByPodConfig, name, overlayPVC string, cmds []workspacekinds.Command, credEnvVar, credSecret, credKey string, deadlineSeconds int64) *batchv1.Job {
	mount := corev1.VolumeMount{Name: "workspace", MountPath: reconcileMountPath}
	var credEnv *corev1.EnvVar
	if credEnvVar != "" && credSecret != "" {
		credEnv = &corev1.EnvVar{
			Name: credEnvVar,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: credSecret},
				Key:                  credKey,
				Optional:             ptr.To(true),
			}},
		}
	}
	inits := make([]corev1.Container, 0, len(cmds))
	for i, c := range cmds {
		if len(c.Argv) == 0 {
			continue
		}
		env := reconcileEnvVars(c.Env)
		if credEnv != nil {
			env = append(env, *credEnv)
		}
		inits = append(inits, corev1.Container{
			Name:         fmt.Sprintf("reconcile-%d", i),
			Image:        cfg.Image,
			Command:      []string{c.Argv[0]},
			Args:         c.Argv[1:],
			Env:          env,
			WorkingDir:   c.Dir,
			VolumeMounts: []corev1.VolumeMount{mount},
		})
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cfg.Namespace, Name: name,
			Labels: map[string]string{"app.kubernetes.io/component": "workspace-reconcile"},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(1)),
			ActiveDeadlineSeconds:   ptr.To(deadlineSeconds),
			TTLSecondsAfterFinished: ptr.To(int32(3600)),
			Template: corev1.PodTemplateSpec{ObjectMeta: jobPodMeta("workspace-reconcile", nil), Spec: corev1.PodSpec{
				RestartPolicy:      corev1.RestartPolicyNever,
				ServiceAccountName: cfg.ServiceAccount,
				InitContainers:     inits,
				Containers: []corev1.Container{{
					Name: "done", Image: cfg.Image, Command: []string{"true"},
					VolumeMounts: []corev1.VolumeMount{mount},
				}},
				Volumes: []corev1.Volume{{
					Name:         "workspace",
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: overlayPVC}},
				}},
			}},
		},
	}
}

func reconcileEnvVars(env []string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			out = append(out, corev1.EnvVar{Name: k, Value: v})
		}
	}
	return out
}
