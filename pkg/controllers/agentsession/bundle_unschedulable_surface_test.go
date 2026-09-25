// pkg/controllers/agentsession/bundle_unschedulable_surface_test.go
//
// Reconcile-level tests for the no-silent-"starting…" surfacing: a bundle or
// detector pod stuck Pending because the scheduler can't place it (e.g.
// "Insufficient cpu") must (1) carry the scheduler's real reason on the
// BundlesReady/RunnerReady condition message — not the generic waiting text —
// and (2) fan a single capacity warning out to the monitoring channel once it
// has been stuck past unschedulableGrace, deduped across the 2s requeue loop.
package agentsession_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"

	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/promptinjection"
)

// monRecorder is a thread-safe channelevents.PublishFunc that decodes every
// published MonitoringEvent for assertion.
type monRecorder struct {
	mu     sync.Mutex
	events []channelevents.MonitoringEvent
}

func (m *monRecorder) publish(_ string, data []byte) error {
	var ev channelevents.MonitoringEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return err
	}
	m.mu.Lock()
	m.events = append(m.events, ev)
	m.mu.Unlock()
	return nil
}

func (m *monRecorder) snapshot() []channelevents.MonitoringEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]channelevents.MonitoringEvent(nil), m.events...)
}

// pendingUnschedulablePod builds a Pending pod with a PodScheduled=False
// condition carrying schedMsg (the scheduler's reason), a stable UID, and a
// fixed CreationTimestamp (so the unschedulableGrace gate is testable).
func pendingUnschedulablePod(name string, uid types.UID, created time.Time, schedMsg string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: uid,
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: corev1.PodReasonUnschedulable, Message: schedMsg,
			}},
		},
	}
}

// pendingNoSchedFailurePod is Pending but schedulable-in-progress (no
// PodScheduled=False) — the fail-safe case that keeps the generic message.
func pendingNoSchedFailurePod(name string, created time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

// bundleWithPod is a not-ready bundle SpiceboxSession whose Status.PodName
// points at podName, so the reconcile collects it into notReadyPods.
func bundleWithPod(sessName, bundleName, podName string, created time.Time) *spiceboxv1alpha1.SpiceboxSession {
	b := notReadyBundle(sessName, bundleName, created)
	b.Status.PodName = podName
	return b
}

func getSession(t *testing.T, c client.Client, name string) spiceboxv1alpha1.AgentSession {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got))
	return got
}

func TestReconcileBundleUnschedulableSurface(t *testing.T) {
	nowT := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }
	sessCreated := nowT.Add(-2 * time.Minute)
	const schedMsg = "0/3 nodes are available: 3 Insufficient cpu."

	cases := []struct {
		name            string
		pod             *corev1.Pod
		podName         string
		wantReasonInMsg bool // BundlesReady message carries the scheduler reason
		wantEvents      int
		// wantScheduling is the expected SandboxScheduling condition status;
		// nil means the condition must not be set yet (no verdict, fail-safe).
		wantScheduling  *metav1.ConditionStatus
		wantSchedReason string
	}{
		{
			name:            "stuck unschedulable past grace: reason in message + one monitoring event",
			pod:             pendingUnschedulablePod("s1-code-pod", "uid-1", nowT.Add(-45*time.Second), schedMsg),
			podName:         "s1-code-pod",
			wantReasonInMsg: true,
			wantEvents:      1,
			wantScheduling:  conditionStatusPtr(metav1.ConditionFalse),
			wantSchedReason: spiceboxv1alpha1.ReasonSandboxUnschedulable,
		},
		{
			name:            "unschedulable but within grace: reason in message, no monitoring event yet",
			pod:             pendingUnschedulablePod("s1-code-pod", "uid-2", nowT.Add(-5*time.Second), schedMsg),
			podName:         "s1-code-pod",
			wantReasonInMsg: true,
			wantEvents:      0,
			wantScheduling:  nil, // stall detected but not yet past grace: no verdict
		},
		{
			name:            "pending but no scheduling failure: generic message, no event (fail-safe)",
			pod:             pendingNoSchedFailurePod("s1-code-pod", nowT.Add(-45*time.Second)),
			podName:         "s1-code-pod",
			wantReasonInMsg: false,
			wantEvents:      0,
			wantScheduling:  conditionStatusPtr(metav1.ConditionTrue),
			wantSchedReason: spiceboxv1alpha1.ReasonSandboxScheduled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := classWithBundle("ac1", spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"})
			sess := sessionCreatedAt("s1", "ac1", sessCreated)
			bundle := bundleWithPod("s1", "code", tc.podName, nowT.Add(-45*time.Second))
			r, c := fakeReconciler(t, clock, ac, sess, bundle, tc.pod)
			rec := &monRecorder{}
			r.MonitoringPublish = rec.publish

			runReconciles(t, r, "s1", 5)

			got := getSession(t, c, "s1")
			br := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionBundlesReady)
			require.NotNil(t, br, "BundlesReady condition set while waiting")
			assert.Equal(t, metav1.ConditionFalse, br.Status)
			assert.Equal(t, "BundlesProvisioning", br.Reason, "reason stays BundlesProvisioning (message-only change)")
			if tc.wantReasonInMsg {
				assert.Contains(t, br.Message, "unschedulable", "message names the scheduling block")
				assert.Contains(t, br.Message, "Insufficient cpu", "message carries the scheduler reason")
			} else {
				assert.Equal(t, "waiting for bundle SpiceboxSessions to become Ready", br.Message,
					"fail-safe: generic message when there is no scheduling failure")
			}

			sc := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSandboxScheduling)
			if tc.wantScheduling == nil {
				assert.Nil(t, sc, "SandboxScheduling should have no verdict yet")
			} else {
				require.NotNil(t, sc, "SandboxScheduling condition set")
				assert.Equal(t, *tc.wantScheduling, sc.Status)
				assert.Equal(t, tc.wantSchedReason, sc.Reason)
			}

			evs := rec.snapshot()
			assert.Len(t, evs, tc.wantEvents, "monitoring events emitted")
			if tc.wantEvents == 1 {
				ev := evs[0]
				assert.Equal(t, channelevents.MonitoringLevelWarning, ev.Level)
				assert.Equal(t, "capacity", ev.Category)
				assert.Equal(t, channelevents.MonitoringTransitionFailed, ev.Transition)
				assert.Equal(t, "AgentSession", ev.Source.Kind)
				assert.Equal(t, "s1", ev.Source.Name)
				assert.Equal(t, spiceboxv1alpha1.AgentSessionConditionBundlesReady, ev.Condition)
				assert.Equal(t, "Unschedulable", ev.Reason)
				assert.Contains(t, ev.Summary, "Insufficient cpu")
				assert.NotEmpty(t, ev.Hint)
			}
		})
	}
}

// conditionStatusPtr is a small literal-address helper for table-driven test
// cases that need a *metav1.ConditionStatus (nil is itself a meaningful
// "no condition yet" expectation, so the field can't be a plain value).
func conditionStatusPtr(s metav1.ConditionStatus) *metav1.ConditionStatus { return &s }

// The 2s bundle requeue loop must fan exactly ONE capacity warning out per
// unschedulable episode, not one per reconcile.
func TestReconcileBundleUnschedulableMonitoringDeduped(t *testing.T) {
	nowT := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }
	ac := classWithBundle("ac1", spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"})
	sess := sessionCreatedAt("s1", "ac1", nowT.Add(-2*time.Minute))
	bundle := bundleWithPod("s1", "code", "s1-code-pod", nowT.Add(-45*time.Second))
	pod := pendingUnschedulablePod("s1-code-pod", "uid-dedupe", nowT.Add(-45*time.Second),
		"0/3 nodes are available: 3 Insufficient cpu.")
	r, _ := fakeReconciler(t, clock, ac, sess, bundle, pod)
	rec := &monRecorder{}
	r.MonitoringPublish = rec.publish

	runReconciles(t, r, "s1", 4)

	assert.Len(t, rec.snapshot(), 1, "exactly one monitoring event across repeated reconciles")
}

// The detector wait surfaces the scheduler reason on RunnerReady the same way.
func TestReconcileDetectorUnschedulableSurface(t *testing.T) {
	nowT := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }
	ac := classWithBundle("acd") // no bundles → straight to the detector gate
	sess := sessionCreatedAt("sd", "acd", nowT.Add(-2*time.Minute))
	det := notReadyDetectorPod("sd", "prompt-injection", nowT.Add(-45*time.Second))
	det.Status.Phase = corev1.PodPending
	det.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable, Message: "0/3 nodes are available: 3 Insufficient cpu.",
	}}
	det.UID = "det-uid-1"
	r, c := fakeReconciler(t, clock, ac, sess, detectorSettings(t), det)
	rec := &monRecorder{}
	r.MonitoringPublish = rec.publish

	runReconciles(t, r, "sd", 5)

	got := getSession(t, c, "sd")
	rr := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady)
	require.NotNil(t, rr, "RunnerReady condition set while awaiting detector")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionAwaitingDetector, rr.Reason)
	assert.Contains(t, rr.Message, "unschedulable", "detector message names the scheduling block")
	assert.Contains(t, rr.Message, "Insufficient cpu", "detector message carries the scheduler reason")

	sc := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSandboxScheduling)
	require.NotNil(t, sc, "SandboxScheduling condition set for a stalled detector pod")
	assert.Equal(t, metav1.ConditionFalse, sc.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonSandboxUnschedulable, sc.Reason)
	assert.Contains(t, sc.Message, "Insufficient cpu")

	evs := rec.snapshot()
	require.Len(t, evs, 1, "one monitoring event for the unschedulable detector")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionConditionRunnerReady, evs[0].Condition)
	assert.Equal(t, "capacity", evs[0].Category)
}

// TestReconcileSandboxSchedulingClearsWhenStallResolves proves the clear side
// of the durable condition end to end: once a bundle pod that was genuinely
// stuck (SandboxScheduling=False) is no longer scheduling-blocked, the very
// next reconcile flips SandboxScheduling back to True/Scheduled — even though
// the bundle SpiceboxSession itself is still not Ready. Without this, the
// in-thread capacity notice channelsd renders from this condition
// would never disappear once shown.
func TestReconcileSandboxSchedulingClearsWhenStallResolves(t *testing.T) {
	ctx := context.Background()
	nowT := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }
	const schedMsg = "0/3 nodes are available: 3 Insufficient cpu."

	ac := classWithBundle("ac1", spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"})
	sess := sessionCreatedAt("s1", "ac1", nowT.Add(-2*time.Minute))
	bundle := bundleWithPod("s1", "code", "s1-code-pod", nowT.Add(-45*time.Second))
	pod := pendingUnschedulablePod("s1-code-pod", "uid-clears", nowT.Add(-45*time.Second), schedMsg)
	r, c := fakeReconciler(t, clock, ac, sess, bundle, pod)

	runReconciles(t, r, "s1", 5)

	got := getSession(t, c, "s1")
	sc := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSandboxScheduling)
	require.NotNil(t, sc, "SandboxScheduling condition set while stuck")
	assert.Equal(t, metav1.ConditionFalse, sc.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonSandboxUnschedulable, sc.Reason)

	// Capacity frees up: the pod no longer carries a PodScheduled=False
	// condition (still Pending, still not Ready — the bundle SpiceboxSession
	// hasn't caught up yet). This is the "no current stall" case, co-located
	// with the stall check in the same reconcile pass that sets BundlesReady.
	var livePod corev1.Pod
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s1-code-pod"}, &livePod))
	livePod.Status.Conditions = nil
	require.NoError(t, c.Status().Update(ctx, &livePod))

	runReconciles(t, r, "s1", 1)

	got = getSession(t, c, "s1")
	sc = apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSandboxScheduling)
	require.NotNil(t, sc, "SandboxScheduling condition still set")
	assert.Equal(t, metav1.ConditionTrue, sc.Status, "clears to True once the stall resolves")
	assert.Equal(t, spiceboxv1alpha1.ReasonSandboxScheduled, sc.Reason)

	// The bundle itself is still not Ready — BundlesReady must stay False,
	// distinguishing "capacity is fine, still starting" from "resolved".
	br := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionBundlesReady)
	require.NotNil(t, br)
	assert.Equal(t, metav1.ConditionFalse, br.Status)
}
