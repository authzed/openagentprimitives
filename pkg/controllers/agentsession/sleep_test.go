// pkg/controllers/agentsession/sleep_test.go
//
// White-box unit test for the pure idle-sleep decision: an Idle channel
// session past its configured sleepAfter grace (and not already slept) is due
// to sleep. Pins the boundary (disabled, no lastIdleAt, already slept,
// within/past grace) without a client or wall-clock.
package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/admission"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TestSandboxProvisioningDesired pins the lazy-provisioning gate: only an
// active/pending turn (Pending/Running) wants bundle SpiceboxSessions
// (re)created. Every parked phase (Idle, AwaitingRetry, AwaitingDecision,
// AwaitingCredentials) and terminal phase (Succeeded, Failed) stays false, so
// a reaped/parked session does not immediately bounce back to full pod count.
func TestSandboxProvisioningDesired(t *testing.T) {
	cases := []struct {
		phase string
		want  bool
	}{
		{spiceboxv1alpha1.AgentSessionPhasePending, true},
		{spiceboxv1alpha1.AgentSessionPhaseRunning, true},
		{spiceboxv1alpha1.AgentSessionPhaseIdle, false},
		{spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry, false},
		{spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision, false},
		{spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, false},
		{spiceboxv1alpha1.AgentSessionPhaseSucceeded, false},
		{spiceboxv1alpha1.AgentSessionPhaseFailed, false},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			assert.Equal(t, tc.want, sandboxProvisioningDesired(tc.phase))
		})
	}
}

func TestIdleSleepDue(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *metav1.Time { t := metav1.NewTime(now.Add(d)); return &t }
	cases := []struct {
		name          string
		sleptAt       *metav1.Time
		lastIdleAt    *metav1.Time
		sleepAfter    time.Duration
		wantDue       bool
		wantRemaining time.Duration
	}{
		{"idle past grace: due", nil, at(-11 * time.Minute), 10 * time.Minute, true, 0},
		{"idle within grace: not due", nil, at(-3 * time.Minute), 10 * time.Minute, false, 7 * time.Minute},
		{"already slept: not due", at(-1 * time.Minute), at(-30 * time.Minute), 10 * time.Minute, false, 0},
		{"sleepAfter<=0 disabled: not due", nil, at(-1 * time.Hour), 0, false, 0},
		{"nil lastIdleAt: not due", nil, nil, 10 * time.Minute, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			due, remaining := idleSleepDue(tc.sleptAt, tc.lastIdleAt, tc.sleepAfter, now)
			assert.Equal(t, tc.wantDue, due)
			assert.Equal(t, tc.wantRemaining, remaining)
		})
	}
}

// withReclaimRequest seeds the fixture session as Admitted=True and carrying
// the admitter's reclaim-requested annotation, then reloads f.sess from the
// fake client so reconcileSleep sees what a reconcile would read.
func (f *sleepFixture) withReclaimRequest(t *testing.T) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	f.sess.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentSessionConditionAdmitted, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonAdmitted, LastTransitionTime: metav1.NewTime(f.now.Add(-time.Hour)),
	}}
	require.NoError(t, f.c.Status().Update(ctx, f.sess), "seed Admitted=True")
	live := f.getSession(t)
	live.Annotations = map[string]string{spiceboxv1alpha1.AnnotationReclaimRequested: f.now.Format(time.RFC3339)}
	require.NoError(t, f.c.Update(ctx, live), "seed reclaim annotation")
	f.sess = f.getSession(t)
}

func TestReconcileSleepReclaimRequested(t *testing.T) {
	cases := []struct {
		name       string
		sleepAfter time.Duration
	}{
		{name: "reclaim annotation on an Idle session within SleepAfter: slept now, Admitted=False/Reclaimed, annotation cleared", sleepAfter: 10 * time.Minute},
		{name: "reclaim annotation with idle sleep disabled: slept now, Admitted=False/Reclaimed, annotation cleared", sleepAfter: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSleepFixture(t, -2*time.Minute, tc.sleepAfter) // idle 2m: not due on its own
			f.withReclaimRequest(t)

			slept, requeueAfter, err := f.r.reconcileSleep(memory.WithSystemApproval(context.Background(), "test"), f.sess, f.class)
			require.NoError(t, err)
			assert.True(t, slept, "a reclaim request makes the sleep due now")
			assert.Zero(t, requeueAfter)

			got := f.getSession(t)
			assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase, "stays Idle (wakeable)")
			assert.NotNil(t, got.Status.SleptAt, "SleptAt stamped")
			adm := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted)
			if assert.NotNil(t, adm, "Admitted condition") {
				assert.Equal(t, metav1.ConditionFalse, adm.Status)
				assert.Equal(t, spiceboxv1alpha1.ReasonAdmissionReclaimed, adm.Reason)
			}
			assert.NotContains(t, got.Annotations, spiceboxv1alpha1.AnnotationReclaimRequested, "annotation cleared")
			assert.True(t, f.bundlesGone(t), "pods reaped")
		})
	}

	t.Run("reclaim annotation on a held session (SessionHold): not slept", func(t *testing.T) {
		f := newSleepFixture(t, -2*time.Minute, 10*time.Minute)
		f.withReclaimRequest(t)
		ctx := memory.WithSystemApproval(context.Background(), "test")
		require.NoError(t, f.c.Create(ctx, &spiceboxv1alpha1.SessionHold{
			ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "default"},
			Spec:       spiceboxv1alpha1.SessionHoldSpec{SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "default", Name: "s1"}},
			Status:     spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseActive},
		}), "create SessionHold")

		slept, _, err := f.r.reconcileSleep(ctx, f.sess, f.class)
		require.NoError(t, err)
		assert.False(t, slept, "the hold guard outranks a reclaim request")

		got := f.getSession(t)
		assert.Nil(t, got.Status.SleptAt, "SleptAt not stamped")
		assert.True(t, admission.IsAdmitted(got), "still admitted")
		assert.False(t, f.bundlesGone(t), "pods kept")
	})

	t.Run("reclaim annotation on an already-slept session: annotation cleared, nothing reaped again", func(t *testing.T) {
		f := newSleepFixture(t, -2*time.Minute, 10*time.Minute)
		f.withReclaimRequest(t)
		ctx := memory.WithSystemApproval(context.Background(), "test")
		sleptAt := metav1.NewTime(f.now.Add(-time.Minute))
		f.sess.Status.SleptAt = &sleptAt
		require.NoError(t, f.c.Status().Update(ctx, f.sess), "seed already-slept status")
		f.sess = f.getSession(t)

		slept, _, err := f.r.reconcileSleep(ctx, f.sess, f.class)
		require.NoError(t, err)
		assert.False(t, slept, "already slept: no second sleep")
		assert.NotContains(t, f.getSession(t).Annotations, spiceboxv1alpha1.AnnotationReclaimRequested, "fulfilled request cleared")
		assert.False(t, f.bundlesGone(t), "not reaped a second time")
	})

	t.Run("stale reclaim annotation on a session that woke (Running): annotation cleared, not slept", func(t *testing.T) {
		f := newSleepFixture(t, -2*time.Minute, 10*time.Minute)
		f.withReclaimRequest(t)
		ctx := memory.WithSystemApproval(context.Background(), "test")
		f.sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
		require.NoError(t, f.c.Status().Update(ctx, f.sess), "seed Running")
		f.sess = f.getSession(t)

		require.NoError(t, f.r.clearStaleReclaimRequest(ctx, f.sess))
		got := f.getSession(t)
		assert.NotContains(t, got.Annotations, spiceboxv1alpha1.AnnotationReclaimRequested, "stale request cleared")
		assert.NotContains(t, f.sess.Annotations, spiceboxv1alpha1.AnnotationReclaimRequested, "in-memory copy agrees")
		assert.Nil(t, got.Status.SleptAt, "not slept")
		assert.False(t, f.bundlesGone(t), "pods kept")
	})

	t.Run("reclaim annotation on an Idle session: clearStaleReclaimRequest leaves it for the sleep path", func(t *testing.T) {
		f := newSleepFixture(t, -2*time.Minute, 10*time.Minute)
		f.withReclaimRequest(t)
		require.NoError(t, f.r.clearStaleReclaimRequest(memory.WithSystemApproval(context.Background(), "test"), f.sess))
		assert.Contains(t, f.getSession(t).Annotations, spiceboxv1alpha1.AnnotationReclaimRequested, "an Idle session's request is not stale")
	})
}
