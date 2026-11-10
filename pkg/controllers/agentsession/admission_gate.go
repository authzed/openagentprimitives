// pkg/controllers/agentsession/admission_gate.go
//
// The reconciler's half of whole-set admission. The admitter decides WHO may
// start (Admitted=True); this file makes the reconciler honour that decision —
// a booting session creates none of its pods until admitted — and hands
// admission back (Admitted=False/Reclaimed) at the moment its pods are
// reclaimed, so the admitter never counts capacity a session no longer holds.
// Its safety net hands admission back too (Admitted=False/PlacementMismatch)
// when an admitted set the scheduler cannot place would otherwise hold its
// placed half while the rest waits.
package agentsession

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/admission"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// awaitingAdmission reports whether this pass must create none of the
// session's pods because it is booting and not yet admitted.
//
// Only a BOOTING session (Pending, or not yet phased) waits. Every pod a
// session owns is created on a booting pass — a parked phase never provisions
// (sandboxProvisioningDesired), and a Running or AwaitingDecision session
// reached that phase through a Pending pass that was already admitted. So
// gating boot alone keeps the invariant "no pod without Admitted=True", while
// a session that was already running when admission was switched on (no
// Admitted condition at all) keeps reconciling its live pods instead of
// freezing mid-turn.
//
// A warm wake — Idle back to Pending while still Admitted=True — is not
// gated: the session never gave up its place, so it provisions at once.
func (r *Reconciler) awaitingAdmission(sess *spiceboxv1alpha1.AgentSession) bool {
	if !r.AdmissionEnabled || admission.IsAdmitted(sess) {
		return false
	}
	return sess.Status.Phase == "" || sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhasePending
}

// waitForAdmission is the whole reconcile tail of a booting session that is
// not admitted: give up once it has waited past the capacity-hold timeout,
// otherwise surface the wait and requeue. It creates nothing.
//
// The wait is measured from admission.WaitingSince — the later of joining the
// line and the last release — so a session released back to the line (a
// placement misfit) gets a full window, not the remainder of its first one.
//
// A negative --capacity-hold-timeout (hold disabled) means no give-up time:
// there is no pod whose ordinary readiness deadline could apply instead, so
// the session waits for admission with no CapacityHold notice.
func (r *Reconciler) waitForAdmission(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (ctrl.Result, error) {
	timeout := r.effectiveCapacityHoldTimeout()
	since := admission.WaitingSince(sess)
	until := since.Add(timeout)
	if timeout > 0 && r.now().After(until) {
		log.FromContext(ctx).Info("session waited past the capacity-hold timeout without admission; failing",
			"session", sess.Namespace+"/"+sess.Name, "class", sess.Spec.Class,
			"waitingSince", since.UTC().Format(time.RFC3339), "timeout", timeout.String())
		r.expireCapacityHold(sess, timeout)
		return r.markBootFailed(ctx, sess, spiceboxv1alpha1.ReasonAgentSessionBundleFail,
			admissionWaitExpiredMessage(timeout))
	}
	requeue := capacityHoldRequeue
	if timeout > 0 {
		r.syncAdmissionHold(sess, until)
		// Wake for the give-up time itself rather than up to a requeue late.
		if remaining := until.Sub(r.now()); remaining > 0 && remaining < requeue {
			requeue = remaining + time.Second
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, r.applyStatus(ctx, sess)
}

// admissionWaitExpiredMessage is the Failed condition message when a session
// waited unadmitted past the capacity-hold timeout. No pod was ever created,
// so unlike capacityHoldExpiredMessage there is no scheduler detail to quote.
func admissionWaitExpiredMessage(waited time.Duration) string {
	return fmt.Sprintf("session waited %s in line for cluster capacity and none freed up; none of its pods were created", waited)
}

// releaseAdmissionOnReclaim records, on sess, that the session's pods are being
// reclaimed: Admitted True→False/Reclaimed. The caller persists it in the same
// status write that records the reclaim. A session that is not Admitted=True
// is left untouched — admission off, or never admitted — so no condition
// appears on sessions that never had one. Reports whether it changed sess.
func (r *Reconciler) releaseAdmissionOnReclaim(sess *spiceboxv1alpha1.AgentSession) bool {
	if !admission.IsAdmitted(sess) {
		return false
	}
	// r.now() for the transition time: admission.JoinedAt and WaitingSince
	// compare it against status.lastWakeAt, and the admitter orders the line
	// by them.
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type:               spiceboxv1alpha1.AgentSessionConditionAdmitted,
		Status:             metav1.ConditionFalse,
		Reason:             spiceboxv1alpha1.ReasonAdmissionReclaimed,
		Message:            "the session's pods were reclaimed; a later wake rejoins the line",
		LastTransitionTime: metav1.NewTime(r.now()),
	})
	return true
}

// admissionMisfitGrace is how long an admitted session's bundle pod may stay
// unschedulable on a capacity shortfall before the session gives its pods back
// and returns to its line. The admitter's model said the whole set fits; a pod
// still unplaced this long later means the model was wrong (a pod it did not
// know about, a node draining), and holding the placed half while the rest
// waits is the hold-and-wait admission exists to end.
const admissionMisfitGrace = 2 * time.Minute

// placementMisfit reports whether this pass must release an admitted session
// whose set the scheduler could not place. stalled is the bundle stage's own
// scheduling-stall verdict (already false for a bundle that regressed
// mid-session, which the ordinary deadline handles).
//
// Only a capacity SHORTFALL counts, for the same reason only a shortfall is
// held (capacityShortfall): a pod blocked by a taint or a volume-affinity
// conflict would be re-admitted at once, fail to place again, and cycle
// forever with its waiting-timeout clock restarting at every release. Left
// alone, it keeps the ordinary readiness deadline and its specific failure
// message.
func (r *Reconciler) placementMisfit(sess *spiceboxv1alpha1.AgentSession, stall schedulingStall, stalled bool) bool {
	if !r.AdmissionEnabled || !stalled || !admission.IsAdmitted(sess) {
		return false
	}
	booting := sess.Status.Phase == "" || sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhasePending
	return booting && capacityShortfall(stall.reason) && stall.pending >= admissionMisfitGrace
}

// releaseMisfit reaps an admitted session's pods and records
// Admitted=False/PlacementMismatch, so the admitter re-queues it. The release
// is written only after the reap succeeds — released while its pods still
// stood, the admitter would count their capacity twice. A failed reap or
// status write leaves the session admitted, and the next pass decides afresh
// from whatever still stands.
//
// The session keeps its place: admission.JoinedAt reads only creation and
// wake times, so it re-joins at the front of its line (it was admitted
// fairly; the model was wrong, not the session). admission.WaitingSince
// restarts at this release, so the capacity-hold timeout gives it a fresh
// window rather than the remainder of its first one.
//
// That fresh window is why the release is bounded by JoinedAt instead. A
// stall the admitter's model cannot see — a shortfall on the only nodes the
// pod tolerates, beside tainted nodes the model still counts — re-admits the
// session at once, stalls again, and releases again: WaitingSince and the
// bundle deadline both restart every cycle, so neither would ever fire. Once
// the session has been in line longer than the capacity-hold timeout since it
// joined, the release becomes the same failure the waiting timeout gives.
//
// The bundle stage's CapacityHold=True ("gives up at …") is released in the
// same write: its give-up time described pods that no longer exist, and left
// standing, every later stall would rewrite it into a fresh "queued" notice.
func (r *Reconciler) releaseMisfit(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, stall schedulingStall) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	timeout := r.effectiveCapacityHoldTimeout()
	joined := admission.JoinedAt(sess)
	expired := timeout > 0 && r.now().Sub(joined) > timeout
	logger.Info("admitted session's pods could not be placed; releasing them",
		"session", sess.Namespace+"/"+sess.Name, "class", sess.Spec.Class,
		"pod", stall.podName, "unschedulableFor", stall.pending.String(),
		"schedulerMessage", stall.reason, "joinedAt", joined.UTC().Format(time.RFC3339),
		"failingAfterCapacityHoldTimeout", expired)
	if _, err := r.reapSessionPods(ctx, sess); err != nil {
		return ctrl.Result{}, err // already logged inside reapSessionPods; retried next pass
	}
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type:               spiceboxv1alpha1.AgentSessionConditionAdmitted,
		Status:             metav1.ConditionFalse,
		Reason:             spiceboxv1alpha1.ReasonAdmissionMisfit,
		Message:            fmt.Sprintf("%s could not be placed for %s (%s); its pods were released", stall.podName, stall.pending.Round(time.Second), strings.TrimSuffix(stall.reason, ".")),
		LastTransitionTime: metav1.NewTime(r.now()),
	})
	if expired {
		r.expireCapacityHold(sess, timeout)
		return r.markBootFailed(ctx, sess, spiceboxv1alpha1.ReasonAgentSessionBundleFail,
			capacityHoldExpiredMessage("sandbox bundle(s)", timeout, stall))
	}
	r.releaseCapacityHold(sess)
	if err := r.applyStatus(ctx, sess); err != nil {
		logger.Info("misfit: recording the admission release failed; will retry",
			"session", sess.Namespace+"/"+sess.Name, "class", sess.Spec.Class, "err", err.Error())
		return ctrl.Result{}, fmt.Errorf("record placement-mismatch release: %w", err)
	}
	return ctrl.Result{RequeueAfter: capacityHoldRequeue}, nil
}
