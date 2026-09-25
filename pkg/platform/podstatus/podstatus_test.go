package podstatus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func waiting(reason string) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "m"}},
		}},
	}}
}

// initWaiting is the sandbox/bundle pod shape: an init container (skill
// unpack, or one per toolchain running a user-supplied image) wedged while the
// app container reports the benign PodInitializing.
func initWaiting(reason string) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "m"}},
		}},
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
		}},
	}}
}

func TestTerminalContainerWaitReason(t *testing.T) {
	cases := []struct {
		name    string
		pod     *corev1.Pod
		wantOK  bool
		wantRsn string
	}{
		{"ImagePullBackOff is terminal", waiting("ImagePullBackOff"), true, "ImagePullBackOff"},
		{"CreateContainerConfigError is terminal", waiting("CreateContainerConfigError"), true, "CreateContainerConfigError"},
		{"CrashLoopBackOff is terminal", waiting("CrashLoopBackOff"), true, "CrashLoopBackOff"},
		{"ContainerCreating is transient (not ok)", waiting("ContainerCreating"), false, ""},
		{"ErrImagePull (first attempt) is transient (not ok)", waiting("ErrImagePull"), false, ""},
		{"no container statuses (still starting) is not ok", &corev1.Pod{}, false, ""},
		{"init container ImagePullBackOff behind PodInitializing is terminal", initWaiting("ImagePullBackOff"), true, "ImagePullBackOff"},
		{"init container CrashLoopBackOff behind PodInitializing is terminal", initWaiting("CrashLoopBackOff"), true, "CrashLoopBackOff"},
		{"init container ContainerCreating is transient (not ok)", initWaiting("ContainerCreating"), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg, ok := TerminalContainerWaitReason(tc.pod)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantRsn, reason)
			if ok {
				assert.Equal(t, "m", msg, "the kubelet message must be carried through for surfacing")
			}
		})
	}
}
