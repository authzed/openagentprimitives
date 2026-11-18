// Package triggeranswer answers the trigger that started a session — a pull
// request's check run, today — on behalf of a session that failed before it
// could answer it itself.
//
// # Who calls this
//
// Whoever moves a session to Failed answers its trigger, at the moment it does
// so. The operator does it when it fails a session itself (a boot failure, the
// maxDuration backstop); the runner does it when IT fails the session (a budget
// cap, a provider error, a halt). There is no third party watching for "any
// Failed session": the runner writes the terminal phase straight onto status,
// so an observer would see only a state, not the transition, and would need a
// durable marker of its own to avoid re-asking the provider on every reconcile
// of every failed session. The writer of the transition already knows it is
// the one transition, so it is the one place this runs exactly once.
//
// # Read before write
//
// Conclude is last-write-wins on the provider. Claim is asked FIRST, and a
// trigger that already carries an answer is left exactly as it is: an agent
// that posted its verdict and then failed on the way out (its budget ran out
// after concluding, say) must not have that verdict replaced with "could not
// finish". When Claim itself fails, whether there is a verdict is UNKNOWN, so
// nothing is written for the same reason.
//
// # Wording
//
// Everything published here is framework-authored and carries the stable
// failure reason token, never the accompanying status message: that message is
// written for whoever runs the agent (it names pods, secrets, kubectl
// commands), and the trigger's surface is read by people who are not that
// person.
package triggeranswer

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// Trigger addresses the trigger that started one session.
type Trigger struct {
	// Client reads the input Channel and its credentials Secret. Resolved at
	// answer time, not captured earlier: a session outlives a credential
	// rotation.
	Client client.Reader

	// Namespace is the session's namespace, where its Channel lives.
	Namespace string

	// Binding is the session's INPUT channel binding — the record of which
	// event started it. Nil for a kubectl-driven session, which no event
	// started and so is owed nothing.
	Binding *spiceboxv1alpha1.ChannelBinding

	// ProviderAPIBaseURL overrides the provider's API host. Empty in
	// production; a test points it at a stand-in.
	ProviderAPIBaseURL string
}

// Result is what AnswerCouldNotFinish did.
type Result string

const (
	// NotOwed: nothing is owed to anyone — no input binding, a kind with no
	// trigger surface, or a binding the kind says carries no trigger.
	NotOwed Result = "not_owed"
	// AlreadyAnswered: the trigger already carried an answer, which was kept.
	AlreadyAnswered Result = "already_answered"
	// Answered: the trigger now reads could_not_finish.
	Answered Result = "answered"
)

// Report describes what AnswerCouldNotFinish observed and did.
type Report struct {
	Result Result
	// Surface names the trigger's surface in the reader's terms; empty when
	// it was never resolved.
	Surface string
	// Standing is the answer that was already there. Set on AlreadyAnswered.
	Standing channelkinds.TriggerOutcome
}

// AnswerCouldNotFinish concludes the trigger as could_not_finish with summary,
// unless it already carries an answer. An error means the trigger was left as
// it was and is still showing the work in progress; the caller logs it and
// carries on, since reaching the terminal state matters more than reporting
// it.
func (t Trigger) AnswerCouldNotFinish(ctx context.Context, summary string) (Report, error) {
	if t.Binding == nil {
		return Report{Result: NotOwed}, nil
	}
	// THE shared lookup, the same one the runner's capability and the two
	// trigger-status tools resolve through. A kind whose trigger is just a
	// message reports nothing here and is untouched.
	reporter, ok := chregistry.TriggerStatusReporterFor(t.Binding.Kind)
	if !ok {
		return Report{Result: NotOwed}, nil
	}
	if t.Client == nil {
		return Report{}, fmt.Errorf("no Kubernetes client to read Channel %s with", t.Binding.Name)
	}

	var ch spiceboxv1alpha1.Channel
	if err := t.Client.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Binding.Name}, &ch); err != nil {
		return Report{}, fmt.Errorf("read the input Channel %s: %w", t.Binding.Name, err)
	}
	var sec corev1.Secret
	if err := t.Client.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: ch.Spec.CredentialsRef.SecretName}, &sec); err != nil {
		return Report{}, fmt.Errorf("read the credentials Secret for Channel %s: %w", t.Binding.Name, err)
	}

	surface, err := reporter.TriggerSurface(&ch, t.Binding, channelkinds.WebhookSecrets{Data: sec.Data},
		channelkinds.TriggerStatusOptions{ProviderAPIBaseURL: t.ProviderAPIBaseURL})
	if err != nil {
		return Report{}, fmt.Errorf("address the trigger of kind %s: %w", t.Binding.Kind, err)
	}
	if surface == nil {
		return Report{Result: NotOwed}, nil // the kind's "this binding carries no trigger" answer
	}
	rep := Report{Surface: surface.Surface()}

	claim, err := surface.Claim(ctx)
	if err != nil {
		return rep, fmt.Errorf("read %s before answering it, so it was left alone: %w", rep.Surface, err)
	}
	if claim.Concluded {
		rep.Result, rep.Standing = AlreadyAnswered, claim.Outcome
		return rep, nil
	}

	if err := surface.Conclude(ctx, channelkinds.TriggerConclusion{
		Outcome: channelkinds.TriggerOutcomeCouldNotFinish,
		Summary: summary,
	}); err != nil {
		return rep, fmt.Errorf("conclude %s: %w", rep.Surface, err)
	}
	rep.Result = Answered
	return rep, nil
}

// Log records what AnswerCouldNotFinish did, for both of its callers, so the
// two cannot drift in what they say. Every outcome but NotOwed is logged: the
// visible symptom of a give-up (a pull request still showing "in progress")
// is otherwise untraceable, and a kept verdict is worth seeing next to a
// failure that might have been expected to overwrite it.
func Log(logger logr.Logger, session, reason string, rep Report, err error) {
	switch {
	case err != nil:
		logger.Info("trigger status: could not answer this failed session's trigger; it keeps showing the work as in progress",
			"session", session, "reason", reason, "surface", rep.Surface, "err", err.Error())
	case rep.Result == AlreadyAnswered:
		logger.Info("trigger status: this trigger already carries an answer; the failure did not overwrite it",
			"session", session, "reason", reason, "surface", rep.Surface, "outcome", string(rep.Standing))
	case rep.Result == Answered:
		logger.Info("trigger status: answered on behalf of a failed session",
			"session", session, "reason", reason, "surface", rep.Surface)
	}
}

// CouldNotStartSummary is the prose published for a session that died before
// its runner came up.
func CouldNotStartSummary(reason string) string {
	return fmt.Sprintf(
		"This agent could not start, so no work was done and none is coming for this commit. "+
			"The operator's record of why is `%s`.", reason)
}

// StoppedSummary is CouldNotStartSummary's counterpart for a session that DID
// start and was ended before it finished — by the operator (its run-time
// budget ran out while its runner could not end it) or by the runner itself (a
// budget cap, a provider error, a halt).
func StoppedSummary(reason string) string {
	return fmt.Sprintf(
		"This agent was stopped before it finished, so its work for this commit is incomplete and no result is coming. "+
			"The operator's record of why is `%s`.", reason)
}
