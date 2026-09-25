package podspec

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// HardenedPodSecurityContext returns the pod-level security context applied to
// both the sandbox pod (Build) and the runner pod (BuildRunnerPod). Callers
// that need the service-account token (e.g. the runner) must leave
// AutomountServiceAccountToken at its default (nil / true); the sandbox pod
// sets it to false separately.
func HardenedPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: ptr.To(true),
		RunAsUser:    ptr.To(int64(1000)),
		RunAsGroup:   ptr.To(int64(1000)),
		FSGroup:      ptr.To(int64(1000)),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// HardenedContainerSecurityContext returns the container-level security
// context applied to both the sandbox container and the runner container.
func HardenedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}
