// pkg/controllers/agentsession/capacity_hold_reconcile_test.go
//
// End-to-end over the reconciler: a bundle pod Pending on a capacity SHORTFALL
// is held past the ordinary 8m deadline instead of failing, and fails only once
// the hold runs out; a released hold restarts the ordinary deadline so a pod
// that just landed on a node is not failed during its image pull.
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

// The scheduler message the zap2 burst produced: one node excluded by volume
// affinity, the rest simply full.
const capacitySchedMsg = "0/6 nodes are available: 1 node(s) didn't match PersistentVolume's node affinity, " +
	"2 Insufficient memory, 5 Insufficient cpu."

func TestReconcileCapacityHold(t *testing.T) {
	nowT := time.Date(2026, 10, 28, 17, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		elapsed     time.Duration // provisioning start → now
		holdTimeout time.Duration // Reconciler.CapacityHoldTimeout; 0 = default (1h)
		// pod builds the bundle pod; created is the provisioning start.
		pod func(created time.Time) *corev1.Pod
		// heldSince pre-seeds an active CapacityHold=True from this long ago.
		heldSince time.Duration
		// wasReady pre-seeds BundlesReady=True: the bundle regressed mid-session.
		wasReady bool

		wantFailed     bool
		wantHoldStatus metav1.ConditionStatus // "" = condition absent
		wantHoldReason string
		wantMsg        []string // substrings of the Failed (or CapacityHold) message
	}{
		{
			name:    "capacity shortfall past the 8m deadline: held, not failed, notice names the give-up time",
			elapsed: 10 * time.Minute,
			pod: func(c time.Time) *corev1.Pod {
				return pendingUnschedulablePod("s1-code-pod", "uid-1", c, capacitySchedMsg)
			},
			wantHoldStatus: metav1.ConditionTrue,
			wantHoldReason: spiceboxv1alpha1.ReasonCapacityHoldWaiting,
			wantMsg:        []string{"out of room", "gives up at 17:50 UTC"},
		},
		{
			name:    "capacity shortfall past the hold timeout: Failed/BundleFailed, hold HoldExpired, message names the wait",
			elapsed: 61 * time.Minute,
			pod: func(c time.Time) *corev1.Pod {
				return pendingUnschedulablePod("s1-code-pod", "uid-1", c, capacitySchedMsg)
			},
			wantFailed:     true,
			wantHoldStatus: metav1.ConditionFalse,
			wantHoldReason: spiceboxv1alpha1.ReasonCapacityHoldExpired,
			wantMsg:        []string{"waited 1h0m0s for cluster capacity", "Insufficient cpu"},
		},
		{
			name:        "hold disabled (negative timeout): fails at the ordinary deadline as before",
			elapsed:     10 * time.Minute,
			holdTimeout: -1,
			pod: func(c time.Time) *corev1.Pod {
				return pendingUnschedulablePod("s1-code-pod", "uid-1", c, capacitySchedMsg)
			},
			wantFailed: true,
			wantMsg:    []string{"did not become Ready within 8m0s", "Insufficient cpu"},
		},
		{
			name:    "non-capacity stall (taint only): not held, fails at the ordinary deadline",
			elapsed: 10 * time.Minute,
			pod: func(c time.Time) *corev1.Pod {
				return pendingUnschedulablePod("s1-code-pod", "uid-1", c,
					"0/6 nodes are available: 6 node(s) had untolerated taint(s).")
			},
			wantFailed: true,
			wantMsg:    []string{"did not become Ready within 8m0s", "untolerated taint"},
		},
		{
			name:    "mid-session regression short of capacity: not held (window already spent), fails as before",
			elapsed: 2 * time.Hour,
			pod: func(c time.Time) *corev1.Pod {
				return pendingUnschedulablePod("s1-code-pod", "uid-1", c, capacitySchedMsg)
			},
			wasReady:   true,
			wantFailed: true,
			wantMsg:    []string{"did not become Ready within 8m0s", "Insufficient cpu"},
		},
		{
			name:    "hold released 30m in (pod now scheduled, pulling): ordinary deadline restarts, not failed",
			elapsed: 30 * time.Minute,
			pod: func(c time.Time) *corev1.Pod {
				return pendingNoSchedFailurePod("s1-code-pod", c)
			},
			heldSince:      25 * time.Minute,
			wantHoldStatus: metav1.ConditionFalse,
			wantHoldReason: spiceboxv1alpha1.ReasonCapacityHoldReleased,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := func() time.Time { return nowT }
			started := nowT.Add(-tc.elapsed)

			ac := classWithBundle("ac1", spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"})
			sess := sessionCreatedAt("s1", "ac1", started)
			if tc.heldSince > 0 {
				sess.Status.Conditions = append(sess.Status.Conditions, metav1.Condition{
					Type:               spiceboxv1alpha1.AgentSessionConditionCapacityHold,
					Status:             metav1.ConditionTrue,
					Reason:             spiceboxv1alpha1.ReasonCapacityHoldWaiting,
					LastTransitionTime: metav1.NewTime(nowT.Add(-tc.heldSince)),
				})
			}
			if tc.wasReady {
				sess.Status.Conditions = append(sess.Status.Conditions, metav1.Condition{
					Type: spiceboxv1alpha1.AgentSessionConditionBundlesReady, Status: metav1.ConditionTrue,
					Reason: "AllBundlesReady", LastTransitionTime: metav1.NewTime(started.Add(time.Minute)),
				})
			}
			bundle := bundleWithPod("s1", "code", "s1-code-pod", started)

			r, c := fakeReconciler(t, clock, ac, sess, bundle, tc.pod(started))
			r.CapacityHoldTimeout = tc.holdTimeout
			runReconciles(t, r, "s1", 5)

			got := getSession(t, c, "s1")
			hold := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCapacityHold)
			if tc.wantHoldStatus == "" {
				assert.Nil(t, hold, "no CapacityHold condition for a session that was never held")
			} else if assert.NotNil(t, hold, "CapacityHold condition") {
				assert.Equal(t, tc.wantHoldStatus, hold.Status)
				assert.Equal(t, tc.wantHoldReason, hold.Reason)
			}

			if !tc.wantFailed {
				assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "session must not fail")
				if hold != nil && hold.Status == metav1.ConditionTrue {
					for _, s := range tc.wantMsg {
						assert.Contains(t, hold.Message, s)
					}
				}
				return
			}
			require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
			assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionBundleFail, got.Status.FailureReason,
				"the reason token stays BundleFailed so a follow-up message still recovers")
			fc := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
			require.NotNil(t, fc, "Failed condition set")
			for _, s := range tc.wantMsg {
				assert.Contains(t, fc.Message, s)
			}
		})
	}
}

// The detector twin: a content-guard detector Pending on a capacity shortfall
// is held past detectorReadyDeadline (5m) rather than failing closed.
func TestReconcileCapacityHold_Detector(t *testing.T) {
	nowT := time.Date(2026, 10, 28, 17, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }
	ac := classWithBundle("acd") // no bundles → straight to the detector gate
	sess := sessionCreatedAt("sd", "acd", nowT.Add(-12*time.Minute))
	det := notReadyDetectorPod("sd", "prompt-injection", nowT.Add(-10*time.Minute))
	det.Status.Phase = corev1.PodPending
	det.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable, Message: "0/3 nodes are available: 3 Insufficient cpu.",
	}}
	r, c := fakeReconciler(t, clock, ac, sess, detectorSettings(t), det)

	runReconciles(t, r, "sd", 5)

	got := getSession(t, c, "sd")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"a detector short of capacity is held, not failed closed at 5m")
	hold := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCapacityHold)
	require.NotNil(t, hold, "CapacityHold set for the stalled detector")
	assert.Equal(t, metav1.ConditionTrue, hold.Status)
}
