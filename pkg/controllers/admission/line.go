// Package admission models the line of AgentSessions waiting to have their
// whole pod set admitted against an AgentClass's capacity. It is pure: no
// client, no clock, so the ordering rules can be tested in isolation.
package admission

import (
	"cmp"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// PodDemand is the resource request of one pod in a session's pod set.
type PodDemand struct {
	Name                               string
	CPUMilli, MemBytes, EphemeralBytes int64
}

// Demand is the whole pod set a session needs admitted together.
type Demand struct {
	Pods []PodDemand
}

// Total sums CPU and memory across the pod set.
func (d Demand) Total() (cpuMilli, memBytes int64) {
	for _, p := range d.Pods {
		cpuMilli += p.CPUMilli
		memBytes += p.MemBytes
	}
	return cpuMilli, memBytes
}

// Candidate is one session in a line. Line is the AgentClass
// "namespace/name" the session draws capacity from.
type Candidate struct {
	Key      types.NamespacedName
	Line     string
	JoinedAt time.Time
	Child    bool
	Demand   Demand
}

func admittedCondition(sess *v1alpha1.AgentSession) *metav1.Condition {
	return meta.FindStatusCondition(sess.Status.Conditions, v1alpha1.AgentSessionConditionAdmitted)
}

// JoinedAt is when the session took its place in line, which orders it there:
// its last wake if it has one, else its creation. A cold wake re-joins the
// back of the line, so it counts; a warm wake never left the line, and a
// release (a placement misfit) keeps the place the session already had. The
// waiting timeout is not measured from here but from gate arrival
// (WaitingSince).
func JoinedAt(sess *v1alpha1.AgentSession) time.Time {
	if wake := sess.Status.LastWakeAt; wake != nil {
		return wake.Time
	}
	return sess.CreationTimestamp.Time
}

// WaitingSince is when the session's current wait began: JoinedAt, or the
// later moment its Admitted condition last went False. The gate stamps
// Admitted=False/Queued at the moment a session reaches it, so for a session
// in line this is its gate arrival — not its creation, however long it was
// parked at a start-approval, identity or credential gate first.
func WaitingSince(sess *v1alpha1.AgentSession) time.Time {
	joined := JoinedAt(sess)
	if c := admittedCondition(sess); c != nil && c.Status == metav1.ConditionFalse &&
		c.LastTransitionTime.Time.After(joined) {
		return c.LastTransitionTime.Time
	}
	return joined
}

// IsQueued reports whether the session has reached the admission gate and is
// waiting in line (Admitted=False/Queued). Only such a session is a candidate
// for admission: one parked at an earlier gate, or blocked before it, has not
// reached the line.
func IsQueued(sess *v1alpha1.AgentSession) bool {
	c := admittedCondition(sess)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == v1alpha1.ReasonAdmissionQueued
}

// PreAdmission reports whether the session started its pods before admission
// was switched on: it has never had an Admitted condition, is not terminal,
// and its status records pods — a runner pod or a bundle SpiceboxSession. The
// reconciler's gate lets such a session through (it already holds what it
// would have been admitted for, and gating it would freeze it with its pods
// up), and the admitter adopts it (Admitted=True/Adopted) so it is counted as
// a holder.
//
// A session whose pods were reaped while it stays non-terminal — slept, or
// Held — is excluded: its status still names the pods it had, but it holds
// nothing, and its next wake must queue like any cold wake.
func PreAdmission(sess *v1alpha1.AgentSession) bool {
	switch sess.Status.Phase {
	case v1alpha1.AgentSessionPhaseSucceeded, v1alpha1.AgentSessionPhaseFailed, v1alpha1.AgentSessionPhaseHeld:
		return false
	}
	if sess.Status.SleptAt != nil || admittedCondition(sess) != nil {
		return false
	}
	if sess.Status.RunnerPodName != "" {
		return true
	}
	for _, b := range sess.Status.BundleSessions {
		if b.SpiceboxSessionName != "" {
			return true
		}
	}
	return false
}

// IsAdmitted reports whether the session's Admitted condition is True.
func IsAdmitted(sess *v1alpha1.AgentSession) bool {
	return meta.IsStatusConditionTrue(sess.Status.Conditions, v1alpha1.AgentSessionConditionAdmitted)
}

// Order sorts cands in place into admission order: children first, then
// earliest JoinedAt, then Key string as a deterministic tiebreak.
func Order(cands []Candidate) {
	slices.SortStableFunc(cands, func(a, b Candidate) int {
		if a.Child != b.Child {
			if a.Child {
				return -1
			}
			return 1
		}
		if c := a.JoinedAt.Compare(b.JoinedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.Key.String(), b.Key.String())
	})
}
