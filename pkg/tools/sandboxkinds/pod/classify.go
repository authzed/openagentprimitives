package pod

import corev1 "k8s.io/api/core/v1"

// oomKilled reports whether any container was killed for exceeding its memory
// limit. The kubelet records this on the terminated container state, not on the
// pod phase, so the pod-level Failed phase alone cannot distinguish an OOM from
// an ordinary crash — and the two need different operator responses.
func oomKilled(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.Reason == "OOMKilled" {
			return true
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
			return true
		}
	}
	return false
}
