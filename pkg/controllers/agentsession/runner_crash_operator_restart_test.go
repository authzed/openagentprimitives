// pkg/controllers/agentsession/runner_crash_operator_restart_test.go
//
// The operator serves the agentsessionidentity admission webhook itself, at
// replicas: 1 with strategy: Recreate. That webhook is failurePolicy: Fail and
// matches exactly the `-runner-sa` principals, so while the operator is being
// replaced — every `oap install`, image bump, eviction, drain — a runner's own
// status and annotation writes are refused. A runner that outlives its retry
// budget in that window exits, and the kubelet restarts it into the same closed
// door.
//
// That makes the RETURNING operator the amplifier: on its first pass it finds a
// crashlooping runner with a captured message and a restart count, and fastFail
// terminalizes the session — for a crash its own absence caused, which its
// return has already fixed. The user's live session dies of a routine upgrade,
// with `failed calling webhook …` relayed into their channel.
//
// So a crash the operator did not witness must not arm the fast path.
package agentsession

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// crashedRunnerPod builds a pod whose runner container is in CrashLoopBackOff
// after terminating non-zero at finishedAt with the given message — the state
// the kubelet publishes for a runner that keeps exiting.
func crashedRunnerPod(t *testing.T, msg string, finishedAt time.Time) *corev1.Pod {
	t.Helper()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session-runner", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         runnerContainerName,
				Ready:        false,
				RestartCount: earlyRunnerCrashRestarts,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
				},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   1,
						Message:    msg,
						FinishedAt: metav1.NewTime(finishedAt),
					},
				},
			}},
		},
	}
}

func TestShouldFastFailRunner(t *testing.T) {
	// The operator came back at this instant; anything before it happened while
	// the operator — and therefore its webhook — was down.
	watchingSince := time.Now()
	const webhookErr = `loop terminal write: Internal error occurred: failed calling webhook ` +
		`"agentsessionidentity.agentprimitives.authzed.com": failed to call webhook: connection refused`

	cases := []struct {
		name     string
		pod      *corev1.Pod
		restarts int32
		crashMsg string
		crashing bool
		want     bool
	}{
		{
			name:     "crash finished before this operator started reconciling: not fast-failed, the outage is a candidate cause",
			pod:      crashedRunnerPod(t, webhookErr, watchingSince.Add(-30*time.Second)),
			restarts: earlyRunnerCrashRestarts,
			crashMsg: webhookErr,
			crashing: true,
			want:     false,
		},
		{
			name:     "crash finished while this operator was watching: fast-failed, the fast path still works",
			pod:      crashedRunnerPod(t, "resolve toolkit \"demo-tools\": not found", watchingSince.Add(5*time.Second)),
			restarts: earlyRunnerCrashRestarts,
			crashMsg: "resolve toolkit \"demo-tools\": not found",
			crashing: true,
			want:     true,
		},
		{
			name:     "witnessed crash but no captured message: not fast-failed, overCap remains the backstop",
			pod:      crashedRunnerPod(t, "", watchingSince.Add(5*time.Second)),
			restarts: earlyRunnerCrashRestarts,
			crashMsg: "",
			crashing: true,
			want:     false,
		},
		{
			name:     "runner recovered: not fast-failed regardless of when it last crashed",
			pod:      crashedRunnerPod(t, webhookErr, watchingSince.Add(5*time.Second)),
			restarts: earlyRunnerCrashRestarts,
			crashMsg: webhookErr,
			crashing: false,
			want:     false,
		},
		{
			name:     "first crash of a fresh pod, witnessed: below the restart threshold, not fast-failed",
			pod:      crashedRunnerPod(t, webhookErr, watchingSince.Add(5*time.Second)),
			restarts: earlyRunnerCrashRestarts - 1,
			crashMsg: webhookErr,
			crashing: true,
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want,
				shouldFastFailRunner(tc.pod, tc.restarts, tc.crashMsg, tc.crashing, watchingSince))
		})
	}
}

// TestWitnessingIsSetOnceAndPresettable pins the two properties the guard rests
// on: the window opens once per operator process (a later call must not slide
// it forward, which would re-arm the fast path against every inherited crash),
// and a preset value wins so the window is controllable.
func TestWitnessingIsSetOnceAndPresettable(t *testing.T) {
	t.Run("unset: recorded once, stable across calls", func(t *testing.T) {
		ticks := []time.Time{time.Unix(1000, 0), time.Unix(2000, 0)}
		var i int
		r := &Reconciler{Now: func() time.Time {
			v := ticks[min(i, len(ticks)-1)]
			i++
			return v
		}}
		first := r.witnessing()
		assert.Equal(t, ticks[0], first, "the window opens at the first Reconcile")
		assert.Equal(t, first, r.witnessing(), "a later call must not slide the window forward")
	})

	t.Run("preset: honored, never overwritten", func(t *testing.T) {
		preset := time.Unix(4242, 0)
		r := &Reconciler{watchingSince: preset, Now: func() time.Time { return time.Unix(9999, 0) }}
		require.Equal(t, preset, r.witnessing(), "a preset window must win over the clock")
	})
}
