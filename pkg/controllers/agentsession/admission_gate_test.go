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
		// wantQueuedAt, when set, is the expected Admitted=False/Queued
		// transition time: the moment the session reached the gate.
		wantQueuedAt time.Time
	}{
		{
			name:    "Pending, queued 2m ago: no SpiceboxSession; CapacityHold=True/WaitingForAdmission after grace",
			enabled: true, bundles: true, createdAt: -2 * time.Minute,
			prep: func(s *spiceboxv1alpha1.AgentSession) {
				withAdmittedCondition(s, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAdmissionQueued, nowT.Add(-2*time.Minute))
			},
			wantHoldStatus: metav1.ConditionTrue,
			wantHoldReason: spiceboxv1alpha1.ReasonCapacityHoldWaitingAdmission,
			wantMsg:        []string{"out of room", "gives up at 17:58 UTC"},
		},
		{
			name:    "Pending, queued 2m ago, no bundles: no runner pod",
			enabled: true, createdAt: -2 * time.Minute,
			prep: func(s *spiceboxv1alpha1.AgentSession) {
				withAdmittedCondition(s, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAdmissionQueued, nowT.Add(-2*time.Minute))
			},
			wantHoldStatus: metav1.ConditionTrue,
			wantHoldReason: spiceboxv1alpha1.ReasonCapacityHoldWaitingAdmission,
		},
		{
			name:    "Pending, not admitted, inside the grace: no pods and no hold notice yet",
			enabled: true, bundles: true, createdAt: -10 * time.Second,
		},
		{
			name:    "first reaches the gate 3h after creation: queued from now, not failed, no hold notice yet",
			enabled: true, bundles: true, createdAt: -3 * time.Hour,
			wantQueuedAt: nowT,
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
			name:    "queued past the timeout: Failed, CapacityHold=False/HoldExpired",
			enabled: true, bundles: true, createdAt: -3 * time.Hour,
			prep: func(s *spiceboxv1alpha1.AgentSession) {
				withAdmittedCondition(s, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAdmissionQueued, nowT.Add(-61*time.Minute))
			},
			wantFailed:     true,
			wantHoldStatus: metav1.ConditionFalse,
			wantHoldReason: spiceboxv1alpha1.ReasonCapacityHoldExpired,
			wantMsg:        []string{"waited 1h0m0s", "cluster capacity"},
		},
		{
			name:    "released long after creation: re-queued at the gate, the wait restarts there, not failed",
			enabled: true, bundles: true, createdAt: -3 * time.Hour,
			prep: func(s *spiceboxv1alpha1.AgentSession) {
				withAdmittedCondition(s, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAdmissionMisfit, nowT.Add(-5*time.Minute))
			},
			wantQueuedAt: nowT,
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

			if !tc.wantQueuedAt.IsZero() {
				adm := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted)
				if assert.NotNil(t, adm, "Admitted condition") {
					assert.Equal(t, metav1.ConditionFalse, adm.Status)
					assert.Equal(t, spiceboxv1alpha1.ReasonAdmissionQueued, adm.Reason)
					assert.Equal(t, tc.wantQueuedAt, adm.LastTransitionTime.Time.UTC(), "queued at gate arrival")
				}
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

// A session that clears its start approval two hours after creation reaches
// the admission gate then, not at creation: it is queued from that moment
// (Admitted=False/Queued at now), is not failed for having "waited" through
// the approval, creates nothing, and provisions normally once admitted.
func TestAdmissionGate_QueuedOnArrivalAfterStartApproval(t *testing.T) {
	nowT := time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }
	codeBundle := spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"}
	ac := classWithBundle("ac1", codeBundle)
	sess := sessionCreatedAt("s1", "ac1", nowT.Add(-2*time.Hour))
	// Approved: channelsd removed the park marker; the phase still says parked.
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingStartApproval
	parked := metav1.NewTime(nowT.Add(-2 * time.Hour))
	sess.Status.StartApprovalParkedAt = &parked

	r, c := fakeReconciler(t, clock, ac, sess)
	r.AdmissionEnabled = true
	runReconciles(t, r, "s1", 3)

	got := getSession(t, c, "s1")
	require.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "an approval wait is not a wait in line")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhasePending, got.Status.Phase)
	adm := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted)
	require.NotNil(t, adm, "Admitted condition written at the gate")
	assert.Equal(t, metav1.ConditionFalse, adm.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonAdmissionQueued, adm.Reason)
	assert.Equal(t, nowT, adm.LastTransitionTime.Time.UTC(), "queued at gate arrival")
	assert.Equal(t, nowT, admission.WaitingSince(&got).UTC(), "the waiting timeout runs from gate arrival")
	assert.False(t, exists(t, c, agentsession.BundleSessionName(&got, codeBundle), &spiceboxv1alpha1.SpiceboxSession{}),
		"no pods before admission")

	// The admitter admits it.
	apimeta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionAdmitted, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonAdmitted, LastTransitionTime: metav1.NewTime(nowT),
	})
	require.NoError(t, c.Status().Update(context.Background(), &got), "admit")
	runReconciles(t, r, "s1", 3)

	got = getSession(t, c, "s1")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
	assert.True(t, exists(t, c, agentsession.BundleSessionName(&got, codeBundle), &spiceboxv1alpha1.SpiceboxSession{}),
		"admitted: the bundle SpiceboxSession is created")
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
// move, and the bundle stage's stale CapacityHold goes with the pods. Short of
// the grace, with admission off, or on a stall another session finishing
// cannot cure, nothing is released. Once the session has been in line longer
// than the capacity-hold timeout since it joined, the release fails it
// instead, so a stall the admitter's model cannot see cannot cycle forever.
func TestAdmissionGate_Misfit(t *testing.T) {
	nowT := time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC)
	codeBundle := spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"}
	// A shortfall on the nodes the pod tolerates, beside a tainted node the
	// admitter's model still counts as room: capacityShortfall says yes, and
	// the model re-admits at once after every release.
	const mixedSchedMsg = "0/4 nodes are available: 3 Insufficient cpu, 1 node(s) had untolerated taint(s)."

	cases := []struct {
		name       string
		enabled    bool          // Reconciler.AdmissionEnabled
		admitted   bool          // the session carries Admitted=True
		pending    time.Duration // how long the bundle pod has been unschedulable
		createdAgo time.Duration // session creation (its JoinedAt); 0 = 20m
		schedMsg   string
		woken      bool // the session was cold-woken before this admission
		heldBefore bool // the bundle stage already set CapacityHold=True/Waiting

		wantReaped      bool
		wantMisfit      bool // Admitted=False/PlacementMismatch at now
		wantFailed      bool // Failed/BundleFailed, CapacityHold=False/HoldExpired
		wantAdmittedNil bool // no Admitted condition at all
	}{
		{
			name:    "admitted, pods unschedulable 2m: pods reaped, released and re-queued, JoinedAt unchanged, CapacityHold released",
			enabled: true, admitted: true, pending: 2 * time.Minute, schedMsg: capacitySchedMsg, heldBefore: true,
			wantReaped: true, wantMisfit: true,
		},
		{
			name:    "admitted after a cold wake, pods unschedulable 2m: released, JoinedAt unchanged",
			enabled: true, admitted: true, pending: 2 * time.Minute, schedMsg: capacitySchedMsg, woken: true,
			wantReaped: true, wantMisfit: true,
		},
		{
			name:    "admitted, mixed taint+Insufficient stall, released and re-admitted repeatedly: fails once JoinedAt+timeout passes",
			enabled: true, admitted: true, pending: 2 * time.Minute, createdAgo: 70 * time.Minute,
			schedMsg: mixedSchedMsg, heldBefore: true,
			wantReaped: true, wantMisfit: true, wantFailed: true,
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
			createdAgo := tc.createdAgo
			if createdAgo == 0 {
				createdAgo = 20 * time.Minute
			}

			ac := classWithBundle("ac1", codeBundle)
			sess := sessionCreatedAt("s1", "ac1", nowT.Add(-createdAgo))
			if tc.woken {
				wake := metav1.NewTime(nowT.Add(-10 * time.Minute))
				sess.Status.LastWakeAt = &wake
			}
			if tc.admitted {
				withAdmittedCondition(sess, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAdmitted, started.Add(-time.Second))
			}
			if tc.heldBefore {
				sess.Status.Conditions = append(sess.Status.Conditions, metav1.Condition{
					Type: spiceboxv1alpha1.AgentSessionConditionCapacityHold, Status: metav1.ConditionTrue,
					Reason:             spiceboxv1alpha1.ReasonCapacityHoldWaiting,
					Message:            "The cluster is out of room right now; this will start on its own once capacity frees up (gives up at 17:58 UTC).",
					LastTransitionTime: metav1.NewTime(started.Add(30 * time.Second)),
				})
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
			assert.Equal(t, joinedBefore.UTC(), admission.JoinedAt(&got).UTC(), "JoinedAt must not move: the session keeps its place in line")

			hold := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCapacityHold)
			if tc.wantFailed {
				require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "a session in line past the timeout fails")
				assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionBundleFail, got.Status.FailureReason)
				if assert.NotNil(t, hold, "CapacityHold condition") {
					assert.Equal(t, metav1.ConditionFalse, hold.Status)
					assert.Equal(t, spiceboxv1alpha1.ReasonCapacityHoldExpired, hold.Reason)
				}
				fc := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
				if assert.NotNil(t, fc, "Failed condition") {
					assert.Contains(t, fc.Message, "waited 1h0m0s for cluster capacity")
					assert.Contains(t, fc.Message, "Insufficient cpu")
				}
			} else {
				assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "session must not fail")
				if tc.wantMisfit && tc.heldBefore && assert.NotNil(t, hold, "CapacityHold condition") {
					assert.Equal(t, metav1.ConditionFalse, hold.Status, "the bundle stage's hold goes with the released pods")
					assert.Equal(t, spiceboxv1alpha1.ReasonCapacityHoldReleased, hold.Reason)
				}
			}

			adm := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted)
			switch {
			case tc.wantAdmittedNil:
				assert.Nil(t, adm, "no Admitted condition")
			case tc.wantMisfit:
				require.NotNil(t, adm, "Admitted condition")
				assert.Equal(t, metav1.ConditionFalse, adm.Status)
				assert.Equal(t, nowT, adm.LastTransitionTime.Time.UTC(), "transition stamped from the reconciler clock")
				if tc.wantFailed {
					assert.Equal(t, spiceboxv1alpha1.ReasonAdmissionMisfit, adm.Reason)
				} else {
					// Released (PlacementMismatch), then back at the gate on the
					// next pass: queued again, its wait restarting there.
					assert.Equal(t, spiceboxv1alpha1.ReasonAdmissionQueued, adm.Reason, "the released session is back in line")
					assert.Equal(t, nowT, admission.WaitingSince(&got).UTC(), "the waiting-timeout clock restarts at the release")
				}
			default:
				require.NotNil(t, adm, "Admitted condition")
				assert.Equal(t, metav1.ConditionTrue, adm.Status, "still admitted")
			}
		})
	}
}
