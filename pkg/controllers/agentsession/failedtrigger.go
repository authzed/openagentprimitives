// pkg/controllers/agentsession/failedtrigger.go
//
// reconcileFailedTrigger is the one place a Failed session's trigger (a pull
// request's check run) is guaranteed an answer.
//
// A session reaches Failed from many writers: the runner itself (a turn or
// token budget, the maxDuration watchdog, a guard halt, a protocol violation),
// the retry TTL and retry-cap paths after a provider error parked it in
// AwaitingRetry, a pod failure, a start-approval timeout. Answering in each of
// them would mean every new Failed writer has to remember to, and the runner
// answering for itself would race this reconciler (both Claim, both see "in
// progress", both Conclude). So the operator owns it: whenever it observes a
// Failed session whose input kind reports a trigger status and that it has not
// yet settled, it answers through triggeranswer — Claim first, so a verdict
// the agent already posted is kept, and an answer the operator's own
// boot-failure or maxDuration path already wrote reads back as AlreadyAnswered.
//
// Settled once, durably: the TriggerAnswered condition is the operator's
// observation in status, and its presence stops every later reconcile of the
// session from reaching the provider at all. A transient failure leaves it
// unset and asks for a retry, so the answer is retried rather than dropped. A
// failure retrying cannot fix (the Channel or its Secret is gone) is settled
// as Unanswerable instead. Neither holds up the session's cleanup: Reconcile
// folds the retry into the terminal block's requeue and goes on to reap.
//
// Bounded by failedTriggerAnswerWindow from finishedAt. Past it nothing is
// written: the pull request may well have moved to a newer commit with a new
// session working on it, and a late "stopped before it finished" could land
// on that session's check run. The same bound is what keeps an upgrade from
// answering every Failed session already in the cluster.
package agentsession

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/triggeranswer"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

const (
	// failedTriggerAnswerWindow is how long after finishedAt the operator
	// will still answer a Failed session's trigger.
	failedTriggerAnswerWindow = time.Hour
	// failedTriggerRetry is the requeue after a failed answer attempt.
	failedTriggerRetry = time.Minute
	// failedTriggerAnswerTimeout bounds one backstop attempt. Tighter than
	// triggerAnswerTimeout because this attempt repeats: a provider that hangs
	// rather than failing costs a reconcile worker this long per failed
	// session per failedTriggerRetry for the whole answer window, and an
	// attempt cut short here is simply retried.
	failedTriggerAnswerTimeout = 10 * time.Second
)

// reconcileFailedTrigger answers sess's trigger if sess is Failed and its
// trigger is still unsettled. It returns a requeue delay when an attempt failed
// and should be retried, else 0. It never fails the reconcile, and the caller
// must not stop on the delay — the session's reap, GC and storage reclaim
// still run in this pass. A status write error is logged and the next
// reconcile re-runs it.
func (r *Reconciler) reconcileFailedTrigger(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) time.Duration {
	if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseFailed {
		return 0
	}
	if conditions.Find(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionTriggerAnswered) != nil {
		return 0 // settled
	}
	// Cheap, local gate first: only a kind that reports a trigger status is
	// ever owed anything, and only those sessions get the marker at all, so a
	// conversational session's reconciles never write here.
	b := sess.Spec.InputChannel
	if b == nil {
		return 0
	}
	if _, ok := chregistry.TriggerStatusReporterFor(b.Kind); !ok {
		return 0
	}

	logger := log.FromContext(ctx)
	sessRef := sess.Namespace + "/" + sess.Name
	reason := sess.Status.FailureReason

	switch {
	case reason == spiceboxv1alpha1.ReasonAgentSessionExpired:
		// The lifetime sweep only reaches Idle sessions, which may already have
		// delivered; it deliberately publishes nothing (reconcileExpiration).
		r.markTriggerSettled(ctx, sess, spiceboxv1alpha1.ReasonTriggerNotOwed,
			"lifetime expiry of an idle session publishes nothing on its trigger")
		return 0
	case sess.Status.SupersededBy != "":
		// A continuation carries the work on; answering here would tell its
		// trigger the work stopped while the child is doing it.
		r.markTriggerSettled(ctx, sess, spiceboxv1alpha1.ReasonTriggerNotOwed,
			"continued by "+sess.Status.SupersededBy)
		return 0
	case sess.Status.FinishedAt == nil || r.now().Sub(sess.Status.FinishedAt.Time) > failedTriggerAnswerWindow:
		logger.Info("trigger status: failed session is past the answer window; its trigger was left as it is",
			"session", sessRef, "reason", reason, "window", failedTriggerAnswerWindow.String())
		r.markTriggerSettled(ctx, sess, spiceboxv1alpha1.ReasonTriggerAnswerWindowElapsed,
			fmt.Sprintf("not answered: more than %s after the session finished", failedTriggerAnswerWindow))
		return 0
	}

	rep, err := r.answerTrigger(ctx, sess, failedTriggerSummary(sess), failedTriggerAnswerTimeout)
	triggeranswer.Log(logger, sessRef, reason, rep, err)
	if triggeranswer.IsUnanswerable(err) {
		// Retrying cannot produce a different result, so settle rather than
		// ask again every failedTriggerRetry until the window closes.
		r.markTriggerSettled(ctx, sess, spiceboxv1alpha1.ReasonTriggerUnanswerable,
			"not answered: "+err.Error())
		return 0
	}
	if err != nil {
		return failedTriggerRetry
	}
	switch rep.Result {
	case triggeranswer.Answered:
		r.markTriggerSettled(ctx, sess, spiceboxv1alpha1.ReasonTriggerAnswered,
			"answered "+rep.Surface+" as could not finish")
	case triggeranswer.AlreadyAnswered:
		r.markTriggerSettled(ctx, sess, spiceboxv1alpha1.ReasonTriggerAlreadyAnswered,
			fmt.Sprintf("%s already carried an answer (%s); it was kept", rep.Surface, rep.Standing))
	default:
		r.markTriggerSettled(ctx, sess, spiceboxv1alpha1.ReasonTriggerNotOwed,
			"this session's binding carries no trigger")
	}
	return 0
}

// failedTriggerSummary picks the published wording from what status records:
// a session that shows no sign of a runner ever having run could not start;
// any other was stopped before it finished. The reason token rides along
// either way.
//
// Any one sign is enough, because none of them is guaranteed on its own: the
// operator's RunnerReady condition can lag a runner that is already working,
// while startedAt and progress are written by the runner itself on its first
// turn.
func failedTriggerSummary(sess *spiceboxv1alpha1.AgentSession) string {
	ran := conditions.Find(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady) != nil ||
		sess.Status.StartedAt != nil || sess.Status.Progress != nil
	if !ran {
		return triggeranswer.CouldNotStartSummary(sess.Status.FailureReason)
	}
	return triggeranswer.StoppedSummary(sess.Status.FailureReason)
}

// markTriggerSettled records the TriggerAnswered observation. A failed write is
// logged, not returned: the next reconcile re-runs the backstop, and Claim's
// read-before-write makes that re-run harmless.
func (r *Reconciler) markTriggerSettled(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, reason, msg string) {
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionTriggerAnswered, Status: metav1.ConditionTrue,
		Reason: reason, Message: msg,
	})
	if err := r.applyStatus(ctx, sess); err != nil {
		log.FromContext(ctx).Info("trigger status: could not record that this failed session's trigger is settled; it will be re-checked",
			"session", sess.Namespace+"/"+sess.Name, "reason", reason, "err", err.Error())
	}
}
