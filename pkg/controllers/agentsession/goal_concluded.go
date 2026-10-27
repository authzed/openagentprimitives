// pkg/controllers/agentsession/goal_concluded.go
//
// concludeGoalRun finishes a goal execution session the goal dispatcher has
// retained (to Succeeded or Failed), so its chain heads are anchored and its
// pods reaped before the dispatcher deletes it at the end of the inspection
// window.
package agentsession

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// concludeGoalRun moves a retained goal run's session to its terminal phase
// and persists status: Succeeded when it ended cleanly from Idle, otherwise
// Failed. The caller has established that the dispatcher retained the run (its
// runner no longer runs) and that the phase is not terminal.
//
// Nothing else would. An ordinary channel session leaves Idle or a parked
// phase through the archive or expiration sweep, but a concluded goal session
// returns before both, so it would sit there until the dispatcher deleted it,
// never anchored, with its sidecar pods still running. ArchiveSweep is the
// wrong event: it parks the session as wakeable, and a goal run that has ended
// must never be resumed.
//
// A SessionHold defers the transition, as it does for the archive sweep:
// reaching terminal is irreversible and evidence outranks cleanup. held
// reports that case.
func (r *Reconciler) concludeGoalRun(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, failed bool) (held bool, err error) {
	hold, err := r.activeHoldFor(ctx, sess)
	if err != nil {
		return false, err
	}
	if hold != nil {
		return true, nil
	}
	var event lifecyclecore.Event = lifecyclecore.GoalRunConcluded{}
	phase, reason, message := spiceboxv1alpha1.AgentSessionPhaseSucceeded, spiceboxv1alpha1.ReasonAgentSessionGoalRunConcluded, "goal run concluded"
	if failed {
		event = lifecyclecore.GoalRunFailed{}
		phase, reason, message = spiceboxv1alpha1.AgentSessionPhaseFailed, spiceboxv1alpha1.ReasonAgentSessionGoalRunFailed, "goal run ended without a clean, reported result"
	}
	if err := r.applyEvent(ctx, sess, event); err != nil {
		return false, err
	}
	// Written directly as well as projected, as in reconcileArchive: the fold
	// may not yet hold the runner's last yield.
	sess.Status.Phase = phase
	sess.Status.FailureReason = ""
	if failed {
		sess.Status.FailureReason = reason
	}
	finished := metav1.Now()
	sess.Status.FinishedAt = &finished
	sess.Status.LastIdleAt = nil
	sess.Status.AwaitingUserInputSince = nil
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionIdle, Status: metav1.ConditionFalse,
		Reason: reason, Message: message,
	})
	terminal := spiceboxv1alpha1.AgentSessionConditionSucceeded
	if failed {
		terminal = spiceboxv1alpha1.AgentSessionConditionFailed
	}
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type: terminal, Status: metav1.ConditionTrue,
		Reason: reason, Message: message,
	})
	return false, r.applyStatus(ctx, sess)
}

// goalSettlePoll is how often an Idle goal session is re-reconciled while it
// waits for the dispatcher's verdict on its run.
const goalSettlePoll = 5 * time.Second

// goalSettleRequeue caps requeue for an Idle goal session at goalSettlePoll.
//
// The dispatcher records a run's outcome and retains it only in its own store,
// and writes nothing to the AgentSession, so no watch event follows. Without
// this, the next reconcile of an Idle goal session is its expiration requeue,
// which lands after the dispatcher has already deleted it at the end of the
// same bound: the run would never be concluded or anchored.
func goalSettleRequeue(sess *spiceboxv1alpha1.AgentSession, requeue time.Duration) time.Duration {
	if sess.Spec.GoalExecution == nil || sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseIdle {
		return requeue
	}
	if requeue == 0 || requeue > goalSettlePoll {
		return goalSettlePoll
	}
	return requeue
}
