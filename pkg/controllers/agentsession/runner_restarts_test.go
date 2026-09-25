// pkg/controllers/agentsession/runner_restarts_test.go
//
// status.runnerRestarts is read by two crashloop guards: the overCap backstop
// (> maxRunnerRestarts) and the fastFail arm (>= earlyRunnerCrashRestarts,
// which is 1). It must therefore describe THE POD BEING REFLECTED, not the
// session's all-time history -- because the runner Pod really is replaced.
// The wake path calls RunnerFactory.Stop, PodRunnerFactory.Stop DELETES the
// Pod, and the next pass Starts a fresh one at the same fixed name with
// RestartCount 0.
//
// Carried forward as a high-water mark, one earlier crash arms fastFail
// VACUOUSLY for the whole remaining life of the session: the first down-window
// of every later fresh pod -- RestartCount still 0, the kubelet has not even
// retried yet -- terminates it as RunnerCrash. That inverts the documented
// intent of earlyRunnerCrashRestarts ("not so eager that a single transient
// crash that would recover is reported").
//
// It must ALSO describe the runner CONTAINER, not the pod. SidecarToolboxes in
// RunModeInPod are real containers in the runner pod, running images taken
// verbatim from a user-supplied CR. Reading restarts or readiness across every
// container makes a third party's flapping MCP server terminate the session as
// a runner crash, and makes a Ready sidecar stand in for a runner that has
// exited. runnerCrashDetail has always filtered by container name; these two
// loops did not.
package agentsession

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// runningPod builds a Running runner pod whose container statuses carry the
// given restart counts (one container per entry, "runner" first).
func runningPod(t *testing.T, restartCounts ...int32) *corev1.Pod {
	t.Helper()
	names := []string{"runner", "sidecar", "detector"}
	var css []corev1.ContainerStatus
	for i, n := range restartCounts {
		name := "extra"
		if i < len(names) {
			name = names[i]
		}
		css = append(css, corev1.ContainerStatus{Name: name, RestartCount: n})
	}
	return &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: css}}
}

// sidecarOnlyPod builds a Running pod whose only container status belongs to a
// sidecar -- no "runner" entry at all.
func sidecarOnlyPod(t *testing.T, restarts int32) *corev1.Pod {
	t.Helper()
	return &corev1.Pod{Status: corev1.PodStatus{
		Phase:             corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{Name: "sidecar", RestartCount: restarts}},
	}}
}

// TestReflectRunnerPod_RunnerReadyTracksTheRunnerContainer pins the readiness
// half of the same defect. With in-pod sidecars a pod is phase=Running as long
// as ANY container runs, so a runner that exited 0 under RestartPolicy:
// OnFailure -- not restarted, gone -- sits in a Running pod beside a Ready
// sidecar. Reading readiness across all containers marks RunnerReady=True,
// which drives the !wasRunnerReady edge to emit RunnerClaimed and move the
// phase to Running, handing the live region to a runner that has exited.
func TestReflectRunnerPod_RunnerReadyTracksTheRunnerContainer(t *testing.T) {
	cases := []struct {
		name  string
		css   []corev1.ContainerStatus
		want  metav1.ConditionStatus
		wantR string
	}{
		{
			name: "runner terminated, sidecar Ready: RunnerReady=False/RunnerCreating",
			css: []corev1.ContainerStatus{
				{Name: "runner", Ready: false},
				{Name: "sidecar-linear", Ready: true},
			},
			want: metav1.ConditionFalse, wantR: spiceboxv1alpha1.ReasonRunnerCreating,
		},
		{
			name: "runner Ready, sidecar not yet: RunnerReady=True/RunnerReady",
			css: []corev1.ContainerStatus{
				{Name: "runner", Ready: true},
				{Name: "sidecar-linear", Ready: false},
			},
			want: metav1.ConditionTrue, wantR: spiceboxv1alpha1.ReasonRunnerReady,
		},
		{
			name: "no runner container status at all: RunnerReady=False/RunnerCreating",
			css:  []corev1.ContainerStatus{{Name: "sidecar-linear", Ready: true}},
			want: metav1.ConditionFalse, wantR: spiceboxv1alpha1.ReasonRunnerCreating,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reconciler{}
			sess := &spiceboxv1alpha1.AgentSession{}
			pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: tc.css}}

			r.reflectRunnerPod(sess, pod)

			cond := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady)
			require.NotNil(t, cond, "RunnerReady condition must be set for a Running pod")
			assert.Equal(t, tc.want, cond.Status)
			assert.Equal(t, tc.wantR, cond.Reason)
		})
	}
}

func TestReflectRunnerPod_RunnerRestartsTracksTheReflectedPod(t *testing.T) {
	cases := []struct {
		name  string
		prior int32
		pod   *corev1.Pod
		want  int32
	}{
		{
			// The defect. Stop deleted the crashed pod; Start made a fresh one
			// at RestartCount 0. Keeping 2 leaves fastFail armed against a pod
			// that has never restarted.
			name:  "fresh pod after a pod replacement: count resets to the new pod's 0",
			prior: 2,
			pod:   runningPod(t, 0),
			want:  0,
		},
		{
			name:  "same pod restarting: count follows the kubelet upward",
			prior: 1,
			pod:   runningPod(t, 3),
			want:  3,
		},
		{
			// Guards the reset: a just-created pod publishes no container
			// statuses at all, and must never be read as "0 restarts" over a
			// live crashloop count.
			name:  "pod with no container statuses yet: prior count is left untouched",
			prior: 2,
			pod:   &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			want:  2,
		},
		{
			// A user-supplied SidecarToolbox in RunModeInPod is a real
			// container in THIS pod, with an image taken verbatim from the CR.
			// Counting its restarts as the runner's terminates the session
			// with "runner restarted 4 times" while the runner never restarted
			// once -- and arms fastFail (threshold 1) on a runner that is fine.
			// runnerCrashDetail already filters by container name; this must
			// too.
			name:  "flapping in-pod sidecar next to a healthy runner: only the runner container counts",
			prior: 0,
			pod:   runningPod(t, 1, 4, 2),
			want:  1,
		},
		{
			// Defensive: a pod that publishes statuses but none for the runner
			// (a name change, a pod that is not ours) must not be read as
			// "0 restarts" over a live crashloop count, same as the
			// no-statuses-at-all case above.
			name:  "container statuses present but none named runner: prior count is left untouched",
			prior: 3,
			pod:   sidecarOnlyPod(t, 5),
			want:  3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reconciler{}
			sess := &spiceboxv1alpha1.AgentSession{
				Status: spiceboxv1alpha1.AgentSessionStatus{RunnerRestarts: tc.prior},
			}

			r.reflectRunnerPod(sess, tc.pod)

			assert.Equal(t, tc.want, sess.Status.RunnerRestarts)
		})
	}
}

// TestReflectRunnerPod_FreshPodDisarmsFastFail states the consequence in the
// terms the guard is written in, so the reason for the reset survives a future
// reader who only looks at this file.
func TestReflectRunnerPod_FreshPodDisarmsFastFail(t *testing.T) {
	r := &Reconciler{}
	sess := &spiceboxv1alpha1.AgentSession{
		// A prior pod generation crashed enough to arm fastFail.
		Status: spiceboxv1alpha1.AgentSessionStatus{RunnerRestarts: earlyRunnerCrashRestarts},
	}

	// The wake path replaced the pod; this one has never restarted.
	r.reflectRunnerPod(sess, runningPod(t, 0))

	assert.Less(t, sess.Status.RunnerRestarts, int32(earlyRunnerCrashRestarts),
		"a fresh pod's first down-window must not fast-fail the session on the previous pod's crash history")
}
