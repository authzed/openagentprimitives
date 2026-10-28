package agentsession

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// Capacity hold: a session whose bundle or detector pod is Pending because the
// cluster is momentarily OUT OF ROOM waits for that room instead of failing at
// the ordinary readiness deadline.
//
// The case it exists for is a burst — a stack of PRs marked ready for review
// at once starts a dozen reviewbot sessions in the same second. The first few
// take every free cpu, the rest go Pending "Insufficient cpu", and at
// bundleReadyDeadline (8m) every one of them failed together, even though the
// earlier sessions were about to finish and free exactly the capacity they
// needed. A Pending pod is already a queue — the scheduler places it the
// moment room appears — so "hold" is nothing more than not giving up on it:
// the session resumes by itself, with no resume step to get wrong.
//
// Only a capacity SHORTFALL is held. A pod that can never fit (its request
// exceeds the largest node) is fast-failed by provablyUnschedulableBundle
// before this is consulted, and a pod blocked only by a taint or a
// volume-affinity conflict will not be unblocked by other sessions finishing,
// so those keep the ordinary deadline and its specific failure message.

// defaultCapacityHoldTimeout bounds a hold when --capacity-hold-timeout is
// unset. Measured from provisioning start, like the readiness deadlines.
const defaultCapacityHoldTimeout = time.Hour

// capacityHoldRequeue is the reconcile cadence while a session is held. A held
// session is not going anywhere until the scheduler acts, and the bundle pod's
// own status change re-triggers the reconcile through the owned SpiceboxSession
// when it does, so the 2s provisioning cadence would only multiply load across
// exactly the burst that caused the hold.
const capacityHoldRequeue = 10 * time.Second

// capacityShortfallMarkers are the scheduler-message fragments that mean "no
// node has enough free X right now" — the shortfall another session finishing
// can cure. The scheduler phrases these per node group ("5 Insufficient cpu",
// "1 Too many pods"), so a substring match is the stable contract.
var capacityShortfallMarkers = []string{"Insufficient ", "Too many pods"}

// capacityShortfall reports whether a scheduler failure message names a
// capacity shortfall on at least one node. A message that ALSO names a
// non-capacity reason for other nodes ("1 node(s) didn't match PersistentVolume's
// node affinity, 5 Insufficient cpu") still counts: the nodes that are merely
// full are the ones that will free up.
func capacityShortfall(schedulerMsg string) bool {
	for _, m := range capacityShortfallMarkers {
		if strings.Contains(schedulerMsg, m) {
			return true
		}
	}
	return false
}

// effectiveCapacityHoldTimeout resolves CapacityHoldTimeout: 0 → default,
// negative → 0 (hold disabled).
func (r *Reconciler) effectiveCapacityHoldTimeout() time.Duration {
	switch {
	case r.CapacityHoldTimeout == 0:
		return defaultCapacityHoldTimeout
	case r.CapacityHoldTimeout < 0:
		return 0
	}
	return r.CapacityHoldTimeout
}

// capacityHoldDeadline returns the readiness deadline to enforce given the
// current scheduling stall, and whether a capacity hold applies. A hold that
// would not EXTEND the base deadline is no hold at all (a slow-cold-start
// storage class's 20m under a 15m hold timeout): the session gets the base
// deadline, no "queued" notice, and — if it times out — the ordinary failure
// message rather than one claiming it was held for capacity.
//
// Only a BOOTING session (Pending, or not yet phased) is ever held. A Running
// session that lost a pod keeps the ordinary fail-closed deadline — its
// content guard or tools are down under live work, and the hold window,
// measured from provisioning start, would misdescribe it anyway — and a
// terminal session has nothing left to wait for (the provisioning stages
// still run on a Failed session's re-reconciles).
func (r *Reconciler) capacityHoldDeadline(sess *spiceboxv1alpha1.AgentSession, stall schedulingStall, stalled bool, base time.Duration) (time.Duration, bool) {
	booting := sess.Status.Phase == "" || sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhasePending
	hold := r.effectiveCapacityHoldTimeout()
	if !booting || !stalled || hold <= base || !capacityShortfall(stall.reason) {
		return base, false
	}
	return hold, true
}

// capacityHoldMessage is the CapacityHold=True Message — the user-facing line
// channelsd posts to the thread. It is a pure function of the provisioning
// start and the deadline (never of now), so the condition is written once per
// episode rather than churned by every requeue.
func capacityHoldMessage(until time.Time) string {
	return fmt.Sprintf("The cluster is out of room right now; this will start on its own once capacity frees up (gives up at %s).",
		until.UTC().Format("15:04 UTC"))
}

// syncCapacityHold records the hold decision on the CapacityHold condition.
// The condition only goes True once the stall has outlived unschedulableGrace —
// the same grace SandboxScheduling uses — so a pod the scheduler places a few
// seconds later never flashes a "queued" notice. Leaving a hold (capacity
// arrived, or the stall turned into something that is not a shortfall) flips a
// True condition to False/CapacityAvailable; a session that was never held
// gets no condition at all.
func (r *Reconciler) syncCapacityHold(sess *spiceboxv1alpha1.AgentSession, held bool, stall schedulingStall, until time.Time) {
	if held {
		if stall.pending >= unschedulableGrace {
			// r.now() for the transition time, like releaseCapacityHold: the
			// episode identity channelsd dedups on is this timestamp, and the
			// restart epoch is compared against r.now().
			conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
				Type:               spiceboxv1alpha1.AgentSessionConditionCapacityHold,
				Status:             metav1.ConditionTrue,
				Reason:             spiceboxv1alpha1.ReasonCapacityHoldWaiting,
				Message:            capacityHoldMessage(until),
				LastTransitionTime: metav1.NewTime(r.now()),
			})
		}
		return
	}
	r.releaseCapacityHold(sess)
}

// releaseCapacityHold flips an active hold to False/CapacityAvailable. A no-op
// when no hold is active, so every "bundles are Ready" pass can call it.
func (r *Reconciler) releaseCapacityHold(sess *spiceboxv1alpha1.AgentSession) {
	if !meta.IsStatusConditionTrue(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCapacityHold) {
		return
	}
	// The transition time is stamped from r.now(), not left to the wall clock:
	// capacityHoldAdjustedStart compares it against provisioning start and
	// r.now(), so all three must come from the same clock.
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type:               spiceboxv1alpha1.AgentSessionConditionCapacityHold,
		Status:             metav1.ConditionFalse,
		Reason:             spiceboxv1alpha1.ReasonCapacityHoldReleased,
		LastTransitionTime: metav1.NewTime(r.now()),
	})
}

// expireCapacityHold records that a hold ran out without capacity arriving.
// Called right before the session is failed, so the terminal notice can say
// "waited an hour for room" instead of blaming the tools.
func (r *Reconciler) expireCapacityHold(sess *spiceboxv1alpha1.AgentSession, waited time.Duration) {
	r.setFalseCondition(sess, spiceboxv1alpha1.AgentSessionConditionCapacityHold,
		spiceboxv1alpha1.ReasonCapacityHoldExpired,
		fmt.Sprintf("waited %s for cluster capacity and none freed up", waited))
}

// capacityHoldExpiredMessage is the Failed condition message when a hold runs
// out: it leads with the wait, then keeps the scheduler's detail for operators.
func capacityHoldExpiredMessage(what string, waited time.Duration, stall schedulingStall) string {
	return fmt.Sprintf("%s waited %s for cluster capacity and none freed up: %s is unschedulable: %s",
		what, waited, stall.podName, strings.TrimSuffix(stall.reason, "."))
}

// capacityHoldAdjustedStart returns the epoch the ORDINARY readiness deadline
// is measured from once a hold has released. Without it, a session held for 40
// minutes whose pod finally lands on a node would be measured against the 8m
// deadline from its original provisioning start — already blown — and fail
// during the very image pull the hold was waiting to begin. Released, the
// deadline restarts from the release instead.
//
// While held (or never held) the start is unchanged: the hold's own window is
// deliberately measured from provisioning start, so "gives up at" stays fixed.
// A hold still True on a pass that is NOT held is being released by this very
// pass (syncCapacityHold runs after the deadline check), so the release time
// is now.
func capacityHoldAdjustedStart(sess *spiceboxv1alpha1.AgentSession, start time.Time, held bool, now time.Time) time.Time {
	if held || start.IsZero() {
		return start
	}
	c := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCapacityHold)
	switch {
	case c == nil:
		return start
	case c.Status == metav1.ConditionTrue:
		return now
	case c.Reason == spiceboxv1alpha1.ReasonCapacityHoldReleased && c.LastTransitionTime.After(start):
		return c.LastTransitionTime.Time
	}
	return start
}
