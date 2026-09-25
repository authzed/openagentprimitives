// pkg/channels/channelsd/pipeline/provider_retry_interaction.go
//
// The bound decision handler for the provider_error_retry interaction category
// — the "Agent failed — Retry" broadcast the session watcher posts when a
// session parks in AwaitingRetry after a provider error. The watcher publishes
// interaction_request(provider_error_retry, AudienceParticipants), the kind's
// generic decision-click handler publishes interaction_decision on the Retry
// click, and this handler applies it.
//
// Standing is NOT re-checked here: HandleInteractionDecision
// (interaction_decision.go) already ran the category's DeciderPolicy check
// (provider_error_retry = DecideParticipant → any user with interact standing
// on the session, fail-closed) before invoking this handler. This handler's
// only job is to wake the parked runner.
package pipeline

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// BindProviderRetryHandler wires decideProviderRetry into the interaction
// registry as the bound decision handler for categories.ProviderErrorRetry.
// Called once at channelsd process start (internal/cmd/channelsd/main.go), after the
// pipeline pl is constructed — mirrors the permission_request Bind site.
func BindProviderRetryHandler(p *Pipeline) {
	channelinteractions.Bind(categories.ProviderErrorRetry, p.decideProviderRetry)
}

// decideProviderRetry applies a resolved provider_error_retry decision: it
// stamps the wake-requested-at annotation via annotateWake so the operator
// respawns the session's exited runner (AwaitingRetry parks with the pod gone),
// then returns Outcome{resolved, "🔄 Retrying…"} — the generic applied path
// renders OutcomeText as the in-place edit that strips the Retry button.
//
// annotateWake is conflict-safe and self-gating: it re-reads the live session
// and no-ops (logging, not erroring) when the session is no longer
// respawn-eligible (already Failed via retry-TTL sweep, or otherwise not
// AwaitingRetry/Idle). So a stale/duplicate click on an already-resolved retry
// is a logged no-op that still renders the resolved outcome — no extra phase
// check is added here.
func (p *Pipeline) decideProviderRetry(ctx context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: d.Session.Namespace, Name: d.Session.Name},
	}
	if err := p.annotateWake(ctx, sess); err != nil {
		return channelinteractions.Outcome{}, fmt.Errorf("provider_error_retry decision: annotate wake (session %s/%s): %w",
			d.Session.Namespace, d.Session.Name, err)
	}
	return channelinteractions.Outcome{Result: channelevents.OutcomeResolved, OutcomeText: "🔄 Retrying…"}, nil
}
