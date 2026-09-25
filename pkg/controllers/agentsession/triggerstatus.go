package agentsession

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// concludeTriggerStatusOnBootFailure answers the trigger that started sess,
// when sess died before anything inside it could.
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
// # Read before write
//
// Conclude is last-write-wins. Claim is asked FIRST, and a trigger that already
// carries an answer is left exactly as it is: a framework cleanup that wrote
// unconditionally would replace a real review's verdict with "could not
// finish", turning a delivered result into a failure notice on the pull
// request. When Claim itself fails, whether there is a verdict is UNKNOWN, so
// nothing is written for the same reason.
//
// Best-effort throughout, and never silent. Nothing here may fail the reconcile
// that is marking the session Failed — reaching the terminal state matters more
// than reporting it, and a session wedged mid-transition by a provider outage
// would be strictly worse. Every give-up path logs, because the visible symptom
// (a pull request showing nothing) is otherwise untraceable.
func (r *Reconciler) concludeTriggerStatusOnBootFailure(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession, reason string,
) {
	logger := log.FromContext(ctx)
	sessRef := sess.Namespace + "/" + sess.Name

	b := sess.Spec.InputChannel
	if b == nil {
		return // kubectl-driven: no channel event started this, so nothing is owed
	}
	// THE shared lookup, the same one the runner's capability and the two
	// trigger-status tools resolve through. A kind whose trigger is just a
	// message reports nothing here and is untouched.
	reporter, ok := chregistry.TriggerStatusReporterFor(b.Kind)
	if !ok {
		return
	}

	var ch spiceboxv1alpha1.Channel
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: b.Name}, &ch); err != nil {
		logger.Info("trigger status: could not read the input Channel, so the trigger keeps showing this session as in progress",
			"session", sessRef, "channel", b.Name, "err", err.Error())
		return
	}
	var sec corev1.Secret
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: ch.Spec.CredentialsRef.SecretName}, &sec); err != nil {
		logger.Info("trigger status: could not read the input Channel's credentials, so the trigger keeps showing this session as in progress",
			"session", sessRef, "channel", b.Name, "err", err.Error())
		return
	}

	// No ProviderAPIBaseURL override: the operator talks to the real provider,
	// and the tests below drive a registered fixture kind that ignores it.
	surface, err := reporter.TriggerSurface(&ch, b, channelkinds.WebhookSecrets{Data: sec.Data},
		channelkinds.TriggerStatusOptions{})
	if err != nil {
		logger.Info("trigger status: this session's trigger could not be addressed",
			"session", sessRef, "kind", b.Kind, "err", err.Error())
		return
	}
	if surface == nil {
		return // the kind's "this binding carries no trigger" answer
	}

	claim, err := surface.Claim(ctx)
	if err != nil {
		logger.Info("trigger status: could not read the trigger before answering it, so it was left alone",
			"session", sessRef, "surface", surface.Surface(), "err", err.Error())
		return
	}
	if claim.Concluded {
		logger.Info("trigger status: this trigger already carries an answer; the boot failure did not overwrite it",
			"session", sessRef, "surface", surface.Surface(), "outcome", string(claim.Outcome))
		return
	}

	if err := surface.Conclude(ctx, channelkinds.TriggerConclusion{
		Outcome: channelkinds.TriggerOutcomeCouldNotFinish,
		Summary: bootFailureSummary(reason),
	}); err != nil {
		logger.Info("trigger status: could not answer this session's trigger; it keeps showing the work as in progress",
			"session", sessRef, "surface", surface.Surface(), "err", err.Error())
		return
	}
	logger.Info("trigger status: answered on behalf of a session that never started",
		"session", sessRef, "surface", surface.Surface(), "reason", reason)
}

// bootFailureSummary is the prose published on the trigger's own surface.
//
// It carries the framework's failure REASON — the stable token that is also on
// status.failureReason — and deliberately not the accompanying operator
// message. That message is written for whoever runs the agent: it names pods,
// secrets and kubectl commands, and this sentence is published to a pull
// request whose readers are not that person and did not choose to trust the
// agent. The reason is what an operator greps for; the first sentence is what
// everyone else actually needs to know.
func bootFailureSummary(reason string) string {
	return fmt.Sprintf(
		"This agent could not start, so no work was done and none is coming for this commit. "+
			"The operator's record of why is `%s`.", reason)
}
