// pkg/controllers/agentsession/bundle_timeout_message_test.go
//
// A bundle that never becomes Ready fails at bundleReadyDeadline. The terminal
// message must reflect WHY: a pod the scheduler cannot place (a taint or a
// volume-node-affinity conflict — neither caught by provablyUnschedulableBundle,
// which only checks the resource ceiling) must NAME that scheduler reason, not
// assert the opposite ("the pod(s) are schedulable, likely a slow image pull").
// That reason is what channelsd relays to the user's thread, so a wrong one
// sends the user chasing an image pull that is not the problem.
package agentsession_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestReconcileBundleTimeout_UnschedulableNamesTheSchedulerReason(t *testing.T) {
	nowT := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }

	// Provisioning started well past bundleReadyDeadline (8m), and the pod is
	// stuck unschedulable for a taint + volume-node-affinity reason —
	// provablyUnschedulableBundle (resource-ceiling only) cannot catch it, so it
	// reaches the deadline backstop.
	started := nowT.Add(-10 * time.Minute)
	const schedMsg = "0/6 nodes are available: 1 node(s) had untolerated taint(s), " +
		"5 node(s) didn't match PersistentVolume's node affinity."

	ac := classWithBundle("ac1", spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"})
	sess := sessionCreatedAt("s1", "ac1", started)
	bundle := bundleWithPod("s1", "code", "s1-code-pod", started)
	pod := pendingUnschedulablePod("s1-code-pod", "uid-1", started, schedMsg)

	r, c := fakeReconciler(t, clock, ac, sess, bundle, pod)
	runReconciles(t, r, "s1", 5)

	got := getSession(t, c, "s1")
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"a bundle stuck past the deadline fails")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionBundleFail, got.Status.FailureReason)

	fc := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	require.NotNil(t, fc, "Failed condition set")
	assert.Contains(t, fc.Message, "unschedulable",
		"terminal message names the scheduling block")
	assert.Contains(t, fc.Message, "untolerated taint",
		"terminal message carries the scheduler's real reason")
	assert.NotContains(t, fc.Message, "the pod(s) are schedulable",
		"must not assert the pod is schedulable when the scheduler says it is not")
	assert.NotContains(t, fc.Message, "image pull",
		"must not misattribute an unschedulable pod to a slow image pull")
}

// The fail-safe direction: when the pod IS scheduled (no PodScheduled=False) but
// just never went Ready, the deadline message should keep pointing at the real
// remaining suspects (image pull / crashing container), not invent a scheduling
// block that isn't there.
func TestReconcileBundleTimeout_ScheduledButNotReadyKeepsImagePullMessage(t *testing.T) {
	nowT := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }
	started := nowT.Add(-10 * time.Minute)

	ac := classWithBundle("ac1", spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"})
	sess := sessionCreatedAt("s1", "ac1", started)
	bundle := bundleWithPod("s1", "code", "s1-code-pod", started)
	pod := pendingNoSchedFailurePod("s1-code-pod", started) // scheduled, just not Ready

	r, c := fakeReconciler(t, clock, ac, sess, bundle, pod)
	runReconciles(t, r, "s1", 5)

	got := getSession(t, c, "s1")
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
	fc := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	require.NotNil(t, fc)
	assert.Contains(t, fc.Message, "image pull",
		"a scheduled-but-not-Ready pod keeps the image-pull/crash explanation")
	assert.NotContains(t, fc.Message, "unschedulable",
		"must not claim a scheduling block for a pod that scheduled fine")
}
