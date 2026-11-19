package agentsession

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/triggeranswer"
)

// triggerAnswerTimeout bounds one answer's provider round trips (a token mint,
// a head-commit read, a check-run read and write), so a slow provider cannot
// hold a reconcile worker. It is the bound for the two immediate answers, each
// of which runs once, in the reconcile that writes Failed. The backstop, which
// retries, runs under the tighter failedTriggerAnswerTimeout.
const triggerAnswerTimeout = 30 * time.Second

// concludeTriggerStatusOnOperatorFailure answers the trigger that started
// sess, at the moment the operator itself fails sess and nothing inside it
// can: either it died before its runner came up (markBootFailed), or its
// runner is wedged past its run-time budget and the operator is ending it
// (reconcileRunDuration). summary is the prose published on the trigger's
// surface; see triggeranswer.CouldNotStartSummary and
// triggeranswer.StoppedSummary.
//
// These two answer immediately, in the reconcile that writes Failed, with the
// wording only they know is right. Every other Failed session — the runner's
// own failures, the retry TTL — is answered by reconcileFailedTrigger, which
// also re-checks these two on the next pass and finds them AlreadyAnswered.
//
// # Why the reconciler and not the agent
//
// Two sessions once died before their first turn — one on credential
// resolution, one on inherited terminal state — and produced no channel message
// and no status on the pull request at all. From the author's side the reviewer
// simply never existed. The agent is the thing that did not start, so the agent
// cannot be the one to say so; the reconciler is the only party left. That is
// why channelkinds.TriggerSurface is built from a Channel, a binding and a
// Secret and from nothing session-runtime, and why Conclude is find-or-create:
// a caller with no session and no runner can still reach it. Resolving a
// Channel's credentials is what materializeSidecarSecret already does in this
// same reconcile, so this needs no RBAC the operator does not hold.
//
// Best-effort throughout, and never silent. Nothing here may fail the reconcile
// that is marking the session Failed — reaching the terminal state matters more
// than reporting it. A give-up here is retried by reconcileFailedTrigger.
func (r *Reconciler) concludeTriggerStatusOnOperatorFailure(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession, reason, summary string,
) {
	rep, err := r.answerTrigger(ctx, sess, summary, triggerAnswerTimeout)
	triggeranswer.Log(log.FromContext(ctx), sess.Namespace+"/"+sess.Name, reason, rep, err)
}

// answerTrigger is the operator's one route to triggeranswer: the session's
// input binding, the operator's own client, the provider override, and bound,
// the deadline over the whole answer.
func (r *Reconciler) answerTrigger(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession, summary string, bound time.Duration,
) (triggeranswer.Report, error) {
	var apiBase string
	if r.TriggerStatusAPIBaseURL != nil {
		apiBase = r.TriggerStatusAPIBaseURL()
	}
	actx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	return triggeranswer.Trigger{
		Client:             r.Client,
		Namespace:          sess.Namespace,
		Binding:            sess.Spec.InputChannel,
		ProviderAPIBaseURL: apiBase,
	}.AnswerCouldNotFinish(actx, summary)
}
