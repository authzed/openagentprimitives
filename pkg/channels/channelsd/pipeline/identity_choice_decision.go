package pipeline

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// IdentityChoiceDecisionHandler is the identity_choice category's bound
// channelinteractions.DecisionHandler, wired via channelinteractions.Bind in
// internal/cmd/channelsd after the pipeline is constructed. HandleInteractionDecision
// owns standing (DecideRequester — only the prompt's addressee may answer),
// idempotency, and publishing Applied on both .in (runner resume) and .out
// (surface ack), so this handler's only job is to validate the 3-way action and
// encode the answer for the runner to read back.
//
// ENCODING: InteractionAppliedPayload has NO Action field (a deliberate
// interaction-model constraint — see channelevents/interaction.go's file-top
// comment). The 3-way answer ("agent" | "userPassthrough" | "cancel") the
// runner's IdentityChoiceGate switches on rides in Outcome.OutcomeText, which
// HandleInteractionDecision copies verbatim into Applied.OutcomeText. The
// runner's .out.interaction_applied subscriber (internal/cmd/runner/main.go,
// subscribeInteractionApplied) reads it back out as approval.Decision.Action.
//
// "cancel" maps to OutcomeDenied (not OutcomeApproved) purely for rendering:
// the webchat InteractionCard and any future decision-aware Slack renderer
// color a "denied" outcome red — the request the user is being asked to
// approve is "let this session use identity X", and cancelling declines that,
// same as any other deny. The gate itself never reads Outcome/Result; it
// switches on the raw action carried in OutcomeText regardless.
func IdentityChoiceDecisionHandler(_ context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
	switch d.Payload.ActionID {
	case "agent", "userPassthrough":
		return channelinteractions.Outcome{
			Result:      channelevents.OutcomeApproved,
			OutcomeText: d.Payload.ActionID,
		}, nil
	case "cancel":
		return channelinteractions.Outcome{
			Result:      channelevents.OutcomeDenied,
			OutcomeText: d.Payload.ActionID,
		}, nil
	default:
		return channelinteractions.Outcome{}, fmt.Errorf("identity_choice decision: unknown action %q", d.Payload.ActionID)
	}
}
