package agentsession

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/triggeranswer"
)

// concludeTriggerStatusOnOperatorFailure answers the trigger that started
// sess, when the operator is failing sess and nothing inside it can: either it
// died before its runner came up (markBootFailed), or its runner is wedged
// past its run-time budget and the operator is ending it (reconcileRunDuration).
// summary is the prose published on the trigger's surface; see
// triggeranswer.CouldNotStartSummary and triggeranswer.StoppedSummary.
//
// The runner answers for the failures IT writes (Loop.fail); this answers for
// the operator's. See package triggeranswer for why the writer of the Failed
// transition is the one that answers, and for the read-before-write rule that
// keeps a verdict already posted.
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
// than reporting it, and a session wedged mid-transition by a provider outage
// would be strictly worse. No ProviderAPIBaseURL override: the operator talks
// to the real provider, and the tests drive a registered fixture kind that
// ignores it.
func (r *Reconciler) concludeTriggerStatusOnOperatorFailure(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession, reason, summary string,
) {
	rep, err := triggeranswer.Trigger{
		Client:    r.Client,
		Namespace: sess.Namespace,
		Binding:   sess.Spec.InputChannel,
	}.AnswerCouldNotFinish(ctx, summary)
	triggeranswer.Log(log.FromContext(ctx), sess.Namespace+"/"+sess.Name, reason, rep, err)
}
