package agentsession

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// defaultStartApprovalTimeout is how long a session created by an org
// non-member may sit parked in AwaitingStartApproval before it fails. A
// generous full day, unlike the identity-choice 30 minutes: the decider is a
// platform admin who may be in another timezone, not the initiating user
// sitting in the thread.
const defaultStartApprovalTimeout = 24 * time.Hour

// reconcileStartApproval is the start-approval gate. It runs BEFORE the two
// identity gates and mirrors their (result, proceed, err) contract:
// proceed=false means the caller returns without running the rest of the
// reconcile — which is precisely what withholds the runner, owner resolution,
// and every provisioning step from a session whose starter has no standing
// yet.
//
// The park signal is channelsd's AnnotationStartApprovalRequestRef, stamped
// on the Create itself, so no version of a parked session ever existed
// without it. Unpark is its removal (decideStartApproval, on Approve): the
// phase returns to Pending and the reconcile proceeds normally. Deny never
// reaches this gate — decideStartApproval sets Status.StartFailure, which the
// StartFailure consumer above this gate drives to Failed.
//
// Unlike AwaitingCredentials/AwaitingIdentityChoice there is no lifecycle-log
// event pair for this park: no runner exists to co-own the state, the phase
// is operator-bookkept on both edges, and a parked reconcile returns before
// the end-of-loop fold could overwrite it.
func (r *Reconciler) reconcileStartApproval(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession,
) (ctrl.Result, bool, error) {
	// A terminal or already-denied session is NEVER (re-)parked, whatever the
	// marker says. The cached client can serve a read where the deny's marker
	// removal has not landed yet while markBootFailed's Failed phase has (or
	// the reverse — StartFailure visible, phase stale): without this guard
	// that stale marker re-parks a Failed session, clobbering the terminal
	// phase back to AwaitingStartApproval — the exact clobber the first live
	// deny run produced. The StartFailure consumer above this gate owns the
	// denied path end to end.
	if isTerminalPhase(sess.Status.Phase) || sess.Status.StartFailure != nil {
		return ctrl.Result{}, true, nil
	}
	if !spiceboxv1alpha1.StartApprovalPending(sess) {
		// Approved (marker removed) while the phase still says parked:
		// return the session to Pending so this same reconcile proceeds
		// through the ordinary start flow.
		if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseAwaitingStartApproval {
			sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
			return ctrl.Result{}, true, r.applyStatus(ctx, sess)
		}
		return ctrl.Result{}, true, nil
	}

	// Marked. Park on first sight: phase + the deadline anchor, in one write.
	changed := false
	if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseAwaitingStartApproval {
		sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingStartApproval
		changed = true
	}
	if sess.Status.StartApprovalParkedAt == nil {
		now := metav1.Now()
		sess.Status.StartApprovalParkedAt = &now
		changed = true
	}
	if changed {
		if err := r.applyStatus(ctx, sess); err != nil {
			return ctrl.Result{}, false, err
		}
	}

	remaining := time.Until(sess.Status.StartApprovalParkedAt.Add(defaultStartApprovalTimeout))
	if remaining <= 0 {
		// Deadline elapsed with no decision → Failed, with a COMPLETE
		// terminal status (phase + reason + finishedAt + condition), exactly
		// as the identity-choice backstop produces: no runner ever existed
		// for this session, so nothing else will ever set these.
		sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
		sess.Status.FailureReason = spiceboxv1alpha1.ReasonAgentSessionStartApprovalTimeout
		now := metav1.Now()
		sess.Status.FinishedAt = &now
		conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
			Type:    spiceboxv1alpha1.AgentSessionConditionFailed,
			Status:  metav1.ConditionTrue,
			Reason:  spiceboxv1alpha1.ReasonAgentSessionStartApprovalTimeout,
			Message: "no platform admin decided the start-approval request before the deadline elapsed",
		})
		return ctrl.Result{}, false, r.applyStatus(ctx, sess)
	}
	return ctrl.Result{RequeueAfter: remaining}, false, nil
}
