package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

func mtime(d time.Duration) *metav1.Time {
	t := metav1.NewTime(at(d))
	return &t
}

func sessionAt(created time.Duration) *v1alpha1.AgentSession {
	s := &v1alpha1.AgentSession{}
	s.CreationTimestamp = *mtime(created)
	return s
}

func withWake(s *v1alpha1.AgentSession, d time.Duration) *v1alpha1.AgentSession {
	s.Status.LastWakeAt = mtime(d)
	return s
}

func withAdmitted(s *v1alpha1.AgentSession, status metav1.ConditionStatus, d time.Duration) *v1alpha1.AgentSession {
	s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.AgentSessionConditionAdmitted,
		Status:             status,
		LastTransitionTime: *mtime(d),
	})
	return s
}

func TestJoinedAt(t *testing.T) {
	cases := []struct {
		name string
		sess *v1alpha1.AgentSession
		want time.Time
	}{
		{"never woke: creationTimestamp", sessionAt(0), at(0)},
		{
			"cold wake after release (Admitted=False at t1, lastWakeAt t2>t1): lastWakeAt",
			withWake(withAdmitted(sessionAt(0), metav1.ConditionFalse, time.Minute), 2*time.Minute),
			at(2 * time.Minute),
		},
		{
			"warm wake while admitted (Admitted=True at t0, lastWakeAt after t0): lastWakeAt",
			withWake(withAdmitted(sessionAt(0), metav1.ConditionTrue, time.Minute), 2*time.Minute),
			at(2 * time.Minute),
		},
		{
			"cold wake, admitted, then released by a misfit (lastWakeAt t1, Admitted=False at t2>t1): lastWakeAt",
			withWake(withAdmitted(sessionAt(0), metav1.ConditionFalse, 2*time.Minute), time.Minute),
			at(time.Minute),
		},
		{
			"wake in the same second as the admission transition: lastWakeAt",
			withWake(withAdmitted(sessionAt(0), metav1.ConditionTrue, time.Minute), time.Minute),
			at(time.Minute),
		},
		{
			"woke with no Admitted condition yet: lastWakeAt",
			withWake(sessionAt(0), time.Minute),
			at(time.Minute),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.True(t, tc.want.Equal(JoinedAt(tc.sess)), "want %s got %s", tc.want, JoinedAt(tc.sess))
		})
	}
}

func TestWaitingSince(t *testing.T) {
	cases := []struct {
		name string
		sess *v1alpha1.AgentSession
		want time.Time
	}{
		{"never admitted: JoinedAt", sessionAt(0), at(0)},
		{
			"released after joining: release time",
			withAdmitted(sessionAt(0), metav1.ConditionFalse, 5*time.Minute),
			at(5 * time.Minute),
		},
		{
			"admitted: JoinedAt, not the admission time",
			withAdmitted(sessionAt(0), metav1.ConditionTrue, 5*time.Minute),
			at(0),
		},
		{
			"queued at the gate two hours after creation (a start approval in between): the gate-arrival time",
			withAdmitted(sessionAt(0), metav1.ConditionFalse, 2*time.Hour),
			at(2 * time.Hour),
		},
		{
			"cold wake after release: the later wake time",
			withWake(withAdmitted(sessionAt(0), metav1.ConditionFalse, time.Minute), 2*time.Minute),
			at(2 * time.Minute),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.True(t, tc.want.Equal(WaitingSince(tc.sess)), "want %s got %s", tc.want, WaitingSince(tc.sess))
		})
	}
}

func TestIsAdmitted(t *testing.T) {
	assert.False(t, IsAdmitted(sessionAt(0)), "no condition: not admitted")
	assert.True(t, IsAdmitted(withAdmitted(sessionAt(0), metav1.ConditionTrue, 0)), "Admitted=True")
	assert.False(t, IsAdmitted(withAdmitted(sessionAt(0), metav1.ConditionFalse, 0)), "Admitted=False")
}

func TestIsQueued(t *testing.T) {
	queued := func(status metav1.ConditionStatus, reason string) *v1alpha1.AgentSession {
		s := sessionAt(0)
		s.Status.Conditions = []metav1.Condition{{
			Type: v1alpha1.AgentSessionConditionAdmitted, Status: status, Reason: reason,
			LastTransitionTime: *mtime(time.Minute),
		}}
		return s
	}
	cases := []struct {
		name string
		sess *v1alpha1.AgentSession
		want bool
	}{
		{"no Admitted condition: not queued", sessionAt(0), false},
		{"Admitted=False/Queued: queued", queued(metav1.ConditionFalse, v1alpha1.ReasonAdmissionQueued), true},
		{"Admitted=False/Reclaimed: not queued (released, has not reached the gate again)", queued(metav1.ConditionFalse, v1alpha1.ReasonAdmissionReclaimed), false},
		{"Admitted=False/PlacementMismatch: not queued", queued(metav1.ConditionFalse, v1alpha1.ReasonAdmissionMisfit), false},
		{"Admitted=True: not queued", queued(metav1.ConditionTrue, v1alpha1.ReasonAdmitted), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsQueued(tc.sess))
		})
	}
}

func TestPreAdmission(t *testing.T) {
	withPods := func(mut func(s *v1alpha1.AgentSession)) *v1alpha1.AgentSession {
		s := sessionAt(0)
		s.Status.Phase = v1alpha1.AgentSessionPhasePending
		mut(s)
		return s
	}
	runner := func(s *v1alpha1.AgentSession) { s.Status.RunnerPodName = "s-runner" }
	bundle := func(s *v1alpha1.AgentSession) {
		s.Status.BundleSessions = []v1alpha1.ResolvedBundle{{Name: "code", SpiceboxSessionName: "s-code"}}
	}
	cases := []struct {
		name string
		sess *v1alpha1.AgentSession
		want bool
	}{
		{"Pending, bundle sessions recorded, no Admitted condition: pre-admission", withPods(bundle), true},
		{"Running, runner pod recorded, no Admitted condition: pre-admission", withPods(func(s *v1alpha1.AgentSession) {
			runner(s)
			s.Status.Phase = v1alpha1.AgentSessionPhaseRunning
		}), true},
		{"warm Idle, runner pod recorded, no Admitted condition: pre-admission", withPods(func(s *v1alpha1.AgentSession) {
			runner(s)
			s.Status.Phase = v1alpha1.AgentSessionPhaseIdle
		}), true},
		{"Pending, nothing recorded: a new session, not pre-admission", withPods(func(*v1alpha1.AgentSession) {}), false},
		{"has an Admitted condition: admission already knows it", withPods(func(s *v1alpha1.AgentSession) {
			bundle(s)
			s.Status.Conditions = []metav1.Condition{{Type: v1alpha1.AgentSessionConditionAdmitted, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonAdmissionQueued}}
		}), false},
		{"slept (pods reaped): not pre-admission", withPods(func(s *v1alpha1.AgentSession) {
			runner(s)
			s.Status.Phase = v1alpha1.AgentSessionPhaseIdle
			s.Status.SleptAt = mtime(time.Minute)
		}), false},
		{"Held (pods reaped): not pre-admission", withPods(func(s *v1alpha1.AgentSession) {
			runner(s)
			s.Status.Phase = v1alpha1.AgentSessionPhaseHeld
		}), false},
		{"terminal: not pre-admission", withPods(func(s *v1alpha1.AgentSession) {
			runner(s)
			s.Status.Phase = v1alpha1.AgentSessionPhaseSucceeded
		}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PreAdmission(tc.sess))
		})
	}
}

func TestDemandTotal(t *testing.T) {
	d := Demand{Pods: []PodDemand{
		{Name: "runner", CPUMilli: 500, MemBytes: 1 << 20, EphemeralBytes: 9},
		{Name: "sandbox", CPUMilli: 250, MemBytes: 2 << 20},
	}}
	cpu, mem := d.Total()
	assert.Equal(t, int64(750), cpu)
	assert.Equal(t, int64(3<<20), mem)
}

func TestOrder(t *testing.T) {
	key := func(n string) types.NamespacedName { return types.NamespacedName{Namespace: "ns", Name: n} }
	cases := []struct {
		name  string
		in    []Candidate
		order []string
	}{
		{
			"children before non-children regardless of time",
			[]Candidate{
				{Key: key("early"), JoinedAt: at(0)},
				{Key: key("late-child"), JoinedAt: at(time.Hour), Child: true},
			},
			[]string{"late-child", "early"},
		},
		{
			"equal JoinedAt breaks on Key.String()",
			[]Candidate{
				{Key: key("b"), JoinedAt: at(0)},
				{Key: key("a"), JoinedAt: at(0)},
			},
			[]string{"a", "b"},
		},
		{
			"otherwise ascending JoinedAt",
			[]Candidate{
				{Key: key("a"), JoinedAt: at(2 * time.Minute)},
				{Key: key("b"), JoinedAt: at(time.Minute)},
				{Key: key("c"), JoinedAt: at(3 * time.Minute), Child: true},
				{Key: key("d"), JoinedAt: at(0), Child: true},
			},
			[]string{"d", "c", "b", "a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			Order(tc.in)
			var got []string
			for _, c := range tc.in {
				got = append(got, c.Key.Name)
			}
			assert.Equal(t, tc.order, got)
		})
	}
}
