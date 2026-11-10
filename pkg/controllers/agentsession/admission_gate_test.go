// pkg/controllers/agentsession/admission_gate_test.go
//
// The admission gate over the reconciler: with AdmissionEnabled, a booting
// session creates none of its pods until the admitter sets Admitted=True, waits
// in line under CapacityHold=True/WaitingForAdmission, and fails once it has
// waited unadmitted past the capacity-hold timeout. Reclaiming a session's pods
// (sleep or terminal reap) releases its admission.
package agentsession_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/admission"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// withAdmittedCondition stamps an Admitted condition onto sess.
func withAdmittedCondition(sess *spiceboxv1alpha1.AgentSession, status metav1.ConditionStatus, reason string, at time.Time) {
	sess.Status.Conditions = append(sess.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionAdmitted, Status: status,
		Reason: reason, LastTransitionTime: metav1.NewTime(at),
	})
}

// exists reports whether obj named name is present in the fake client.
func exists(t *testing.T, c client.Client, name string, obj client.Object) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err, "get %s", name)
	return true
}

func TestAdmissionGate(t *testing.T) {
	nowT := time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC)
	codeBundle := spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"}

	cases := []struct {
		name      string
		enabled   bool          // Reconciler.AdmissionEnabled
		bundles   bool          // class has a tool bundle (else: the runner is the first pod)
		createdAt time.Duration // session creation, relative to now
		// prep stamps admission state onto the session before it is loaded.
		prep func(sess *spiceboxv1alpha1.AgentSession)

		wantPods       bool // the bundle SpiceboxSession (bundles) or runner pod (no bundles) exists
		wantFailed     bool
		wantHoldStatus metav1.ConditionStatus // "" = CapacityHold absent
		wantHoldReason string
		wantMsg        []string // substrings of the CapacityHold (held) or Failed message
	}{
		{
			name:    "Pending, not admitted: no SpiceboxSession; CapacityHold=True/WaitingForAdmission after grace",
			enabled: true, bundles: true, createdAt: -2 * time.Minute,
			wantHoldStatus: metav1.ConditionTrue,
			wantHoldReason: spiceboxv1alpha1.ReasonCapacityHoldWaitingAdmission,
			wantMsg:        []string{"out of room", "gives up at 17:58 UTC"},
		},
		{
			name:    "Pending, not admitted, no bundles: no runner pod",
			enabled: true, createdAt: -2 * time.Minute,
			wantHoldStatus: metav1.ConditionTrue,
			wantHoldReason: spiceboxv1alpha1.ReasonCapacityHoldWaitingAdmission,
		},
		{
			name:    "Pending, not admitted, inside the grace: no pods and no hold notice yet",
			enabled: true, bundles: true, createdAt: -10 * time.Second,
		},
		{
			name:    "Pending, admitted: bundle SpiceboxSession created as today, no hold",
			enabled: true, bundles: true, createdAt: -2 * time.Minute,
			prep: func(s *spiceboxv1alpha1.AgentSession) {
				withAdmittedCondition(s, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAdmitted, nowT.Add(-time.Minute))
			},
			wantPods: true,
		},
		{
			name:    "Pending, admitted, no bundles: runner pod created as today",
			enabled: true, createdAt: -2 * time.Minute,
			prep: func(s *spiceboxv1alpha1.AgentSession) {
				withAdmittedCondition(s, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAdmitted, nowT.Add(-time.Minute))
			},
			wantPods: true,
		},
		{
			name:    "warm wake (Pending again with Admitted still True): provisions with no hold",
			enabled: true, bundles: true, createdAt: -3 * time.Hour,
			prep: func(s *spiceboxv1alpha1.AgentSession) {
				withAdmittedCondition(s, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAdmitted, nowT.Add(-2*time.Hour))
				wake := metav1.NewTime(nowT.Add(-time.Minute))
				s.Status.LastWakeAt = &wake
			},
			wantPods: true,
		},
		{
			name:    "admitted after a held wait: the WaitingForAdmission hold is released, pods created",
			enabled: true, bundles: true, createdAt: -20 * time.Minute,
			prep: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{
					Type: spiceboxv1alpha1.AgentSessionConditionCapacityHold, Status: metav1.ConditionTrue,
					Reason:             spiceboxv1alpha1.ReasonCapacityHoldWaitingAdmission,
					LastTransitionTime: metav1.NewTime(nowT.Add(-19 * time.Minute)),
				})
				withAdmittedCondition(s, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAdmitted, nowT.Add(-time.Second))
			},
			wantPods:       true,
			wantHoldStatus: metav1.ConditionFalse,
			wantHoldReason: spiceboxv1alpha1.ReasonCapacityHoldReleased,
		},
		{
			name:    "waiting past the timeout from join time: Failed, CapacityHold=False/HoldExpired",
			enabled: true, bundles: true, createdAt: -61 * time.Minute,
			wantFailed:     true,
			wantHoldStatus: metav1.ConditionFalse,
			wantHoldReason: spiceboxv1alpha1.ReasonCapacityHoldExpired,
			wantMsg:        []string{"waited 1h0m0s", "cluster capacity"},
		},
		{
			name:    "released long after creation: the wait is measured from the release, not failed",
			enabled: true, bundles: true, createdAt: -3 * time.Hour,
			prep: func(s *spiceboxv1alpha1.AgentSession) {
				withAdmittedCondition(s, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAdmissionMisfit, nowT.Add(-5*time.Minute))
			},
			wantHoldStatus: metav1.ConditionTrue,
			wantHoldReason: spiceboxv1alpha1.ReasonCapacityHoldWaitingAdmission,
			wantMsg:        []string{"gives up at 17:55 UTC"},
		},
		{
			name:    "AdmissionEnabled=false: bundle SpiceboxSession created without Admitted",
			bundles: true, createdAt: -2 * time.Minute,
			wantPods: true,
		},
		{
			name:      "AdmissionEnabled=false, no bundles: runner pod created without Admitted",
			createdAt: -2 * time.Minute,
			wantPods:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := func() time.Time { return nowT }
			var ac *spiceboxv1alpha1.AgentClass
			if tc.bundles {
				ac = classWithBundle("ac1", codeBundle)
			} else {
				ac = classWithBundle("ac1")
			}
			sess := sessionCreatedAt("s1", "ac1", nowT.Add(tc.createdAt))
			if tc.prep != nil {
				tc.prep(sess)
			}

			r, c := fakeReconciler(t, clock, ac, sess)
			r.AdmissionEnabled = tc.enabled
			runReconciles(t, r, "s1", 3)

			got := getSession(t, c, "s1")
			if tc.bundles {
				assert.Equal(t, tc.wantPods,
					exists(t, c, agentsession.BundleSessionName(&got, codeBundle), &spiceboxv1alpha1.SpiceboxSession{}),
					"bundle SpiceboxSession present")
			} else {
				assert.Equal(t, tc.wantPods,
					exists(t, c, agentsession.RunnerPodName(&got), &corev1.Pod{}),
					"runner pod present")
			}

			hold := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCapacityHold)
			if tc.wantHoldStatus == "" {
				assert.Nil(t, hold, "no CapacityHold condition")
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
			assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionBundleFail, got.Status.FailureReason)
			fc := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
			require.NotNil(t, fc, "Failed condition set")
			for _, s := range tc.wantMsg {
				assert.Contains(t, fc.Message, s)
			}
		})
	}
}

// A terminal reap releases admission in the status write that accompanies it,
// and never invents an Admitted condition on a session that never had one.
func TestAdmissionGate_TerminalReapReleases(t *testing.T) {
	nowT := time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC)
	cases := []struct {
		name         string
		admitted     bool
		wantAdmitted *metav1.ConditionStatus // nil = condition absent
	}{
		{name: "admitted Succeeded session reaped: Admitted=False/Reclaimed", admitted: true, wantAdmitted: ptrTo(metav1.ConditionFalse)},
		{name: "never-admitted Succeeded session reaped: no Admitted condition written"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := func() time.Time { return nowT }
			sess := sessionCreatedAt("s1", "ac1", nowT.Add(-time.Hour))
			sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
			finished := metav1.NewTime(nowT.Add(-time.Minute))
			sess.Status.FinishedAt = &finished
			if tc.admitted {
				withAdmittedCondition(sess, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAdmitted, nowT.Add(-50*time.Minute))
			}
			r, c := fakeReconciler(t, clock, sess)
			r.AdmissionEnabled = true
			runReconciles(t, r, "s1", 3)

			got := getSession(t, c, "s1")
			adm := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted)
			if tc.wantAdmitted == nil {
				assert.Nil(t, adm, "no Admitted condition")
				return
			}
			require.NotNil(t, adm, "Admitted condition")
			assert.Equal(t, *tc.wantAdmitted, adm.Status)
			assert.Equal(t, spiceboxv1alpha1.ReasonAdmissionReclaimed, adm.Reason)
			assert.Equal(t, nowT, adm.LastTransitionTime.Time.UTC(), "transition stamped from the reconciler clock")
		})
	}
}

// The sleep reap stamps SleptAt and releases admission in the same status
// write (one WriteOwned): a wake clears SleptAt, so a release written
// separately could be lost between the two, and the session would sit
// admitted with no pods.
func TestAdmissionGate_SleepReleasesInOneWrite(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	nowT := time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }

	idle := metav1.NewTime(nowT.Add(-time.Hour))
	sess := sessionCreatedAt("s1", "ac1", nowT.Add(-2*time.Hour))
	sess.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "chan-1", Kind: "fake", Key: "thread-1"}
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseIdle
	sess.Status.LastIdleAt = &idle
	withAdmittedCondition(sess, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAdmitted, nowT.Add(-90*time.Minute))
	ac := classWithBundle("ac1")

	r, c := fakeReconciler(t, clock, ac, sess)
	r.AdmissionEnabled = true
	r.DefaultSessionSleepAfter = 10 * time.Minute

	live := getSession(t, c, "s1")
	slept, _, err := r.ReconcileSleepForTest(ctx, &live, ac)
	require.NoError(t, err)
	require.True(t, slept, "past-grace Idle session sleeps")

	got := getSession(t, c, "s1")
	assert.NotNil(t, got.Status.SleptAt, "SleptAt stamped")
	adm := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted)
	require.NotNil(t, adm, "Admitted condition")
	assert.Equal(t, metav1.ConditionFalse, adm.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonAdmissionReclaimed, adm.Reason)
	assert.Equal(t, nowT, adm.LastTransitionTime.Time.UTC(), "transition stamped from the reconciler clock")
}

func ptrTo[T any](v T) *T { return &v }

// An ADMITTED booting session whose bundle pod the scheduler still cannot
// place after admissionMisfitGrace gives its pods back and returns to its line
// (Admitted=False/PlacementMismatch), keeping its place: JoinedAt does not
// move. Short of the grace, with admission off, or on a stall another session
// finishing cannot cure, nothing is released.
func TestAdmissionGate_Misfit(t *testing.T) {
	nowT := time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC)
	codeBundle := spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"}

	cases := []struct {
		name     string
		enabled  bool          // Reconciler.AdmissionEnabled
		admitted bool          // the session carries Admitted=True
		pending  time.Duration // how long the bundle pod has been unschedulable
		schedMsg string
		woken    bool // the session was cold-woken before this admission

		wantReaped      bool
		wantMisfit      bool // Admitted=False/PlacementMismatch at now
		wantAdmittedNil bool // no Admitted condition at all
	}{
		{
			name:    "admitted, pods unschedulable 2m: pods reaped, Admitted=False/PlacementMismatch, JoinedAt unchanged",
			enabled: true, admitted: true, pending: 2 * time.Minute, schedMsg: capacitySchedMsg,
			wantReaped: true, wantMisfit: true,
		},
		{
			name:    "admitted after a cold wake, pods unschedulable 2m: released, JoinedAt unchanged",
			enabled: true, admitted: true, pending: 2 * time.Minute, schedMsg: capacitySchedMsg, woken: true,
			wantReaped: true, wantMisfit: true,
		},
		{
			name:    "admitted, pods unschedulable 1m (inside the misfit grace): pods kept, still admitted",
			enabled: true, admitted: true, pending: time.Minute, schedMsg: capacitySchedMsg,
		},
		{
			name:    "admitted, taint-only stall 2m: not a capacity misfit, pods kept, still admitted",
			enabled: true, admitted: true, pending: 2 * time.Minute,
			schedMsg: "0/6 nodes are available: 6 node(s) had untolerated taint(s).",
		},
		{
			name:     "admission off, pods unschedulable 2m: pods kept under the pod-level hold, no Admitted condition",
			pending:  2 * time.Minute,
			schedMsg: capacitySchedMsg, wantAdmittedNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := func() time.Time { return nowT }
			started := nowT.Add(-tc.pending)

			ac := classWithBundle("ac1", codeBundle)
			sess := sessionCreatedAt("s1", "ac1", nowT.Add(-20*time.Minute))
			if tc.woken {
				wake := metav1.NewTime(nowT.Add(-10 * time.Minute))
				sess.Status.LastWakeAt = &wake
			}
			if tc.admitted {
				withAdmittedCondition(sess, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAdmitted, started.Add(-time.Second))
			}
			joinedBefore := admission.JoinedAt(sess)
			bundle := bundleWithPod("s1", "code", "s1-code-pod", started)
			pod := pendingUnschedulablePod("s1-code-pod", "uid-1", started, tc.schedMsg)

			r, c := fakeReconciler(t, clock, ac, sess, bundle, pod)
			r.AdmissionEnabled = tc.enabled
			runReconciles(t, r, "s1", 3)

			got := getSession(t, c, "s1")
			assert.Equal(t, !tc.wantReaped,
				exists(t, c, agentsession.BundleSessionName(&got, codeBundle), &spiceboxv1alpha1.SpiceboxSession{}),
				"bundle SpiceboxSession present")
			assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "session must not fail")
			assert.Equal(t, joinedBefore.UTC(), admission.JoinedAt(&got).UTC(), "JoinedAt must not move: the session keeps its place in line")

			adm := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted)
			switch {
			case tc.wantAdmittedNil:
				assert.Nil(t, adm, "no Admitted condition")
			case tc.wantMisfit:
				require.NotNil(t, adm, "Admitted condition")
				assert.Equal(t, metav1.ConditionFalse, adm.Status)
				assert.Equal(t, spiceboxv1alpha1.ReasonAdmissionMisfit, adm.Reason)
				assert.Equal(t, nowT, adm.LastTransitionTime.Time.UTC(), "transition stamped from the reconciler clock")
				assert.Equal(t, nowT, admission.WaitingSince(&got).UTC(), "the waiting-timeout clock restarts at the release")
			default:
				require.NotNil(t, adm, "Admitted condition")
				assert.Equal(t, metav1.ConditionTrue, adm.Status, "still admitted")
			}
		})
	}
}
