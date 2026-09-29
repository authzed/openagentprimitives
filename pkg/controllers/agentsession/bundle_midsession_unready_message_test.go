// pkg/controllers/agentsession/bundle_midsession_unready_message_test.go
//
// A bundle that became Ready and later turned unhealthy mid-session (an
// OOM-killed container, an evicted pod) lands in the same not-ready deadline
// backstop as a bundle that never booted — the deadline is measured from the
// bundle's CreationTimestamp, so mid-session it is trivially exceeded on the
// first not-ready pass. The terminal message must not describe that as a
// provisioning failure ("did not become Ready within 8m0s of provisioning
// start … likely a slow/cold image pull"): in a real incident the images were
// cached, the bundle was Ready in 13 seconds, and the wording sent the
// operator chasing an image pull for what was a cgroup OOM kill 35 minutes
// into the session. The session's own BundlesReady=True condition is the
// durable record that boot succeeded; when it is set, the message must say the
// bundle became unhealthy after running, not that it never started.
package agentsession_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// runningNotReadyPod is a scheduled, Running pod whose Ready condition is
// False — the state kubelet reports while a killed container restarts. Any
// container statuses given are attached verbatim.
func runningNotReadyPod(name string, created time.Time, cs ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: cs,
		},
	}
}

// midSessionUnreadyFixture builds the incident shape: a 35m-old session whose
// bundles all became Ready 34m ago (the durable BundlesReady condition), with
// the bundle now not-Ready and its pod Running but unready.
func midSessionUnreadyFixture(nowT time.Time, pod *corev1.Pod) (*spiceboxv1alpha1.AgentClass, *spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.SpiceboxSession) {
	started := nowT.Add(-35 * time.Minute)
	ac := classWithBundle("ac1", spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"})
	sess := sessionCreatedAt("s1", "ac1", started)
	sess.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentSessionConditionBundlesReady, Status: metav1.ConditionTrue,
		Reason:             "AllBundlesReady",
		LastTransitionTime: metav1.NewTime(nowT.Add(-34 * time.Minute)),
	}}
	bundle := bundleWithPod("s1", "code", pod.Name, started)
	return ac, sess, bundle
}

func TestReconcileBundleTimeout_MidSessionUnreadyIsNotBlamedOnProvisioning(t *testing.T) {
	nowT := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }

	pod := runningNotReadyPod("s1-code-pod", nowT.Add(-35*time.Minute))
	ac, sess, bundle := midSessionUnreadyFixture(nowT, pod)

	r, c := fakeReconciler(t, clock, ac, sess, bundle, pod)
	runReconciles(t, r, "s1", 5)

	got := getSession(t, c, "s1")
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"a mid-session bundle death still fails the session (recovery semantics are separate)")
	// The Reason token must stay the exact "BundleFailed" constant — it is the
	// lookup key lifecycle's transientBootFailures uses to classify a follow-up
	// message as recoverable.
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionBundleFail, got.Status.FailureReason)

	fc := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	require.NotNil(t, fc, "Failed condition set")
	assert.Contains(t, fc.Message, "became unhealthy",
		"message says the bundle died after running, not that it never started")
	assert.Contains(t, fc.Message, "s1-code", "message names the offending bundle")
	assert.Contains(t, fc.Message, "kubectl -n default describe pod s1-code-pod",
		"message keeps the actionable kubectl pointer")
	assert.NotContains(t, fc.Message, "image pull",
		"a bundle that was Ready for half an hour must not be blamed on an image pull")
	assert.NotContains(t, fc.Message, "did not become Ready within",
		"must not use boot-deadline framing for a mid-session death")
}

// When kubelet has already recorded WHY the container died — the
// LastTerminationState of a restarting container, e.g. OOMKilled/137 — the
// message must name that observed cause instead of listing guesses.
func TestReconcileBundleTimeout_MidSessionUnreadyNamesContainerTermination(t *testing.T) {
	nowT := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }

	pod := runningNotReadyPod("s1-code-pod", nowT.Add(-35*time.Minute),
		// A successfully-Completed container (exit 0) must not be mistaken
		// for the failure.
		corev1.ContainerStatus{
			Name: "setup",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "Completed", ExitCode: 0,
			}},
		},
		corev1.ContainerStatus{
			Name:         "sandbox",
			RestartCount: 1,
			State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "OOMKilled", ExitCode: 137,
			}},
		})
	ac, sess, bundle := midSessionUnreadyFixture(nowT, pod)

	r, c := fakeReconciler(t, clock, ac, sess, bundle, pod)
	runReconciles(t, r, "s1", 5)

	got := getSession(t, c, "s1")
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
	fc := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	require.NotNil(t, fc, "Failed condition set")
	assert.Contains(t, fc.Message, "OOMKilled",
		"message names the container's actual recorded termination reason")
	assert.Contains(t, fc.Message, `container "sandbox"`,
		"message names which container died, not the Completed one")
	assert.Contains(t, fc.Message, "exit code 137")
	assert.NotContains(t, fc.Message, "likely",
		"an observed termination replaces the guess list")
}

// The boot path gets the same honesty: a bundle that NEVER became Ready keeps
// the provisioning-deadline framing, but when its container is crash-looping
// with a recorded termination, the message names that exit instead of blaming
// a slow image pull.
func TestReconcileBundleTimeout_BootCrashLoopNamesContainerTermination(t *testing.T) {
	nowT := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }
	started := nowT.Add(-10 * time.Minute)

	ac := classWithBundle("ac1", spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"})
	sess := sessionCreatedAt("s1", "ac1", started) // no BundlesReady=True: never booted
	bundle := bundleWithPod("s1", "code", "s1-code-pod", started)
	pod := runningNotReadyPod("s1-code-pod", started, corev1.ContainerStatus{
		Name:         "sandbox",
		RestartCount: 4,
		State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error", ExitCode: 1,
		}},
	})

	r, c := fakeReconciler(t, clock, ac, sess, bundle, pod)
	runReconciles(t, r, "s1", 5)

	got := getSession(t, c, "s1")
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
	fc := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	require.NotNil(t, fc, "Failed condition set")
	assert.Contains(t, fc.Message, "did not become Ready within",
		"a never-Ready bundle keeps the provisioning-deadline framing")
	assert.Contains(t, fc.Message, `container "sandbox"`)
	assert.Contains(t, fc.Message, "exit code 1",
		"message names the recorded crash instead of guessing")
	assert.NotContains(t, fc.Message, "image pull",
		"an observed crash replaces the image-pull guess")
}
