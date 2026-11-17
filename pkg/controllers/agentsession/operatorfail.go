package agentsession

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// failSessionByOperator moves a live session to Failed on the operator's own
// authority (a budget it is enforcing in place of the runner): records ev in
// the signed lifecycle log, stamps phase, failureReason, finishedAt and the
// Failed condition on sess, persists them, and then answers the session's
// trigger, since a session failed from outside has a runner that will not.
//
// The write goes through applyStatus, not a WriteOwned against sess: earlier
// steps of this reconcile may have mutated sess without writing (a re-derived
// audit key, for one), and diffing against the already-mutated object would
// drop those changes. A Failed session's later reconciles return at the
// terminal reap block before re-deriving them, so they would stay lost.
//
// The trigger answer runs only after the write succeeds, so a transition that
// did not persist publishes nothing, and it is best-effort and read-before-write
// (see concludeTriggerStatusOnOperatorFailure): a verdict the runner already
// posted is kept.
func (r *Reconciler) failSessionByOperator(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ev lifecyclecore.Event, reason, msg string,
) error {
	if err := r.applyEvent(ctx, sess, ev); err != nil {
		return err
	}
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
	sess.Status.FailureReason = reason
	finished := metav1.NewTime(r.now())
	sess.Status.FinishedAt = &finished
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionFailed, Status: metav1.ConditionTrue,
		Reason: reason, Message: msg,
	})
	if err := r.applyStatus(ctx, sess); err != nil {
		return err
	}
	r.concludeTriggerStatusOnOperatorFailure(ctx, sess, reason, operatorStoppedSummary(reason))
	return nil
}
