package agentsession

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

// runnerCrashDetail is what lets the reconciler put the runner's REAL startup
// error on the user's channel (instead of a generic "restarted N times") and
// surface it fast — so these cases pin the WHETHER (crashing vs recovered) and
// the WHAT (the captured message).
func TestRunnerCrashDetail(t *testing.T) {
	runnerStatus := func(s corev1.ContainerStatus) *corev1.Pod {
		s.Name = "runner"
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{s}}}
	}

	t.Run("terminated non-zero: returns the captured message + crashing", func(t *testing.T) {
		pod := runnerStatus(corev1.ContainerStatus{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Message: "  toolkit \"sre\" did not resolve\n",
			}},
		})
		msg, crashing := runnerCrashDetail(pod)
		assert.True(t, crashing)
		assert.Equal(t, `toolkit "sre" did not resolve`, msg)
	})

	t.Run("CrashLoopBackOff: reads the prior termination message", func(t *testing.T) {
		pod := runnerStatus(corev1.ContainerStatus{
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Message: "boom"}},
		})
		msg, crashing := runnerCrashDetail(pod)
		assert.True(t, crashing)
		assert.Equal(t, "boom", msg)
	})

	t.Run("recovered (Ready) despite a prior crash: not crashing", func(t *testing.T) {
		// A transient crash that recovered must NOT be surfaced as a terminal
		// failure — the container is Ready now even though LastTermination
		// records the earlier exit.
		pod := runnerStatus(corev1.ContainerStatus{
			Ready:                true,
			State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "old blip"}},
		})
		msg, crashing := runnerCrashDetail(pod)
		assert.False(t, crashing)
		assert.Empty(t, msg)
	})

	t.Run("clean exit (0): not crashing", func(t *testing.T) {
		pod := runnerStatus(corev1.ContainerStatus{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: "done"}},
		})
		_, crashing := runnerCrashDetail(pod)
		assert.False(t, crashing)
	})

	t.Run("no runner container: not crashing", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "sidecar", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "x"}}},
		}}}
		_, crashing := runnerCrashDetail(pod)
		assert.False(t, crashing)
	})
}
