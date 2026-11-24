package agentsession

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/triggeranswer"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

// triggerAnswerTimeout bounds one answer's provider round trips (a token mint,
// a head-commit read, a check-run read and write), so a slow provider cannot
// hold a reconcile worker. It is the bound for the two immediate answers, each
// of which runs once, in the reconcile that writes Failed. The backstop, which
// retries, runs under the tighter failedTriggerAnswerTimeout.
const triggerAnswerTimeout = 30 * time.Second

// channelOwnerKind is the owner kind the Channel controller adopts a
// credentials Secret under. An answer that has to adopt one adopts it under
// the same kind and the same Channel, so both write the same thing.
const channelOwnerKind = "Channel"

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
// input binding, the operator's own client, a live read of the credentials
// Secret, the provider override, and bound, the deadline over the whole
// answer.
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
		Secrets:            r.triggerCredentialsSecret(sess),
		Namespace:          sess.Namespace,
		Binding:            sess.Spec.InputChannel,
		ProviderAPIBaseURL: apiBase,
	}.AnswerCouldNotFinish(actx, summary)
}

// triggerCredentialsSecret is how an answer reads the input Channel's
// credentials Secret: live, never through r.Client.
//
// r.Client's Secret cache is label-filtered and holds only adopted Secrets, so
// a Secret that exists but is not adopted yet reads back from it as NotFound.
// One recreated during a key rotation is in that state until the Channel
// controller next reconciles, and a session that failed in the meantime would
// be settled for good as unanswerable, with a message saying the Secret does
// not exist. So existence is asked of the API server, and the Secret is
// adopted first when nothing has adopted it, since the guarded reader refuses
// an unadopted one. It is adopted for the Channel that references it, which is
// exactly what the Channel controller's own next reconcile applies: no
// per-session owner is added to a Secret every session of the Channel shares.
//
// NotFound from here means the Secret is really gone. Any other failure,
// adoption included, is returned as it is and retried.
func (r *Reconciler) triggerCredentialsSecret(sess *spiceboxv1alpha1.AgentSession) triggeranswer.SecretGetter {
	return func(ctx context.Context, nn types.NamespacedName) (*corev1.Secret, error) {
		if r.SecretReader == nil || sess.Spec.InputChannel == nil {
			return r.getSecret(ctx, nn)
		}
		channel := types.NamespacedName{Namespace: sess.Namespace, Name: sess.Spec.InputChannel.Name}
		adopted, err := adoptkit.AdoptSecretIfUnadopted(ctx, r.SecretReader.Reader, r.Client, nn, channel, channelOwnerKind)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil, err
			}
			return nil, fmt.Errorf("adopt it for Channel %s before reading it: %w", channel.Name, err)
		}
		if adopted {
			log.FromContext(ctx).Info("trigger status: adopted the input Channel's credentials Secret, which nothing had adopted yet, to answer a failed session's trigger",
				"session", sess.Namespace+"/"+sess.Name, "channel", channel.Name, "secret", nn.Name)
		}
		return r.SecretReader.Get(ctx, nn)
	}
}
