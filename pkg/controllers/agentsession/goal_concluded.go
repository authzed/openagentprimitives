// pkg/controllers/agentsession/goal_concluded.go
//
// concludeGoalRun finishes a goal execution session the goal dispatcher has
// retained (Idle → Succeeded), so its chain heads are anchored before the
// dispatcher deletes it at the end of the inspection window.
package agentsession

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// concludeGoalRun marks a retained goal run's session Succeeded and persists
// status. The caller has established that the occurrence is retained (the run
// ended and every runner pod succeeded) and that the phase is not terminal.
//
// A goal run normally ends with the runner's Idle yield. An ordinary channel
// session would later leave Idle through the archive or expiration sweep, but a
// concluded goal session returns before both, so without this it would sit Idle
// until the dispatcher deleted it and never be anchored. ArchiveSweep is the
// wrong event here: it parks the session as wakeable, and a goal run that has
// ended must never be resumed.
//
// A SessionHold defers the transition, as it does for the archive sweep:
// reaching terminal is irreversible and evidence outranks cleanup.
func (r *Reconciler) concludeGoalRun(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	hold, err := r.activeHoldFor(ctx, sess)
	if err != nil {
		return err
	}
	if hold != nil {
		return nil
	}
	if err := r.applyEvent(ctx, sess, lifecyclecore.GoalRunConcluded{}); err != nil {
		return err
	}
	// Written directly as well as projected, as in reconcileArchive: the fold
	// may not yet hold the runner's IdleYield.
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	sess.Status.FailureReason = ""
	finished := metav1.Now()
	sess.Status.FinishedAt = &finished
	sess.Status.LastIdleAt = nil
	sess.Status.AwaitingUserInputSince = nil
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionIdle, Status: metav1.ConditionFalse,
		Reason:  spiceboxv1alpha1.ReasonAgentSessionGoalRunConcluded,
		Message: "goal run concluded",
	})
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionSucceeded, Status: metav1.ConditionTrue,
		Reason:  spiceboxv1alpha1.ReasonAgentSessionGoalRunConcluded,
		Message: "goal run concluded",
	})
	return r.applyStatus(ctx, sess)
}
