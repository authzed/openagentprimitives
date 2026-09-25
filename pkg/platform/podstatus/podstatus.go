// Package podstatus holds small, controller-agnostic readers over a Pod's
// runtime status. It exists so multiple controllers (agentsession runner/sidecar
// pods, spiceboxsession bundle pods) classify a Pod's "stuck" states the same
// way instead of each re-deriving the rules.
package podstatus

import corev1 "k8s.io/api/core/v1"

// terminalContainerWaitReasons are kubelet container "Waiting" reasons a pod
// will NOT recover from on its own: a bad or inaccessible image, an
// unsatisfiable container config, or a container that keeps crashing. They are
// distinct from the benign transient reasons (ContainerCreating, PodInitializing,
// the first ErrImagePull before back-off) that resolve by themselves.
var terminalContainerWaitReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"CrashLoopBackOff":           true,
	"InvalidImageName":           true,
	"ImageInspectError":          true,
	"ErrImageNeverPull":          true,
	"CreateContainerConfigError": true,
}

// TerminalContainerWaitReason returns the first container Waiting state whose
// reason is terminal (see terminalContainerWaitReasons), so a caller can fail
// closed in seconds instead of waiting out a multi-minute readiness deadline. ok
// is false when the pod is merely still starting. This is a no-silent-hang
// lever: a wedged pod (e.g. an unpullable image → ImagePullBackOff) surfaces its
// real cause immediately rather than looking like a generic timeout minutes
// later.
//
// Init containers are scanned FIRST, where this matters most: sandbox and
// bundle pods run user-supplied images as init containers (skill unpack, one
// per toolchain), and a wedged init container leaves the app container
// reporting only the benign PodInitializing — so scanning ContainerStatuses
// alone reports "still starting" for a pod that never will.
func TerminalContainerWaitReason(pod *corev1.Pod) (reason, message string, ok bool) {
	for _, css := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range css {
			if w := cs.State.Waiting; w != nil && terminalContainerWaitReasons[w.Reason] {
				return w.Reason, w.Message, true
			}
		}
	}
	return "", "", false
}
