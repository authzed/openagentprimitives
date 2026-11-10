// pkg/controllers/agentsession/admission_gate.go
//
// The reconciler's half of whole-set admission. The admitter decides WHO may
// start (Admitted=True); this file makes the reconciler honour that decision —
// a booting session creates none of its pods until admitted — and hands
// admission back (Admitted=False/Reclaimed) at the moment its pods are
// reclaimed, so the admitter never counts capacity a session no longer holds.
package agentsession

import (
	"context"
	"fmt"
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
