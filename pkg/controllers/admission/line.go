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

// JoinedAt is when the session took its place in line. A wake that came after
// the last admission transition re-queues the session, so it counts; a wake
// the session was later released after (e.g. a placement misfit) does not, and
// the session keeps its original place at creationTimestamp.
func JoinedAt(sess *v1alpha1.AgentSession) time.Time {
	wake := sess.Status.LastWakeAt
	if wake == nil {
		return sess.CreationTimestamp.Time
	}
	if c := admittedCondition(sess); c != nil && !wake.Time.After(c.LastTransitionTime.Time) {
		return sess.CreationTimestamp.Time
	}
	return wake.Time
}

// WaitingSince is when the session's current wait began: JoinedAt, or the
// later moment it was released from admission.
func WaitingSince(sess *v1alpha1.AgentSession) time.Time {
	joined := JoinedAt(sess)
	if c := admittedCondition(sess); c != nil && c.Status == metav1.ConditionFalse &&
		c.LastTransitionTime.Time.After(joined) {
		return c.LastTransitionTime.Time
	}
	return joined
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
