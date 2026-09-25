package pipeline

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// ApprovalDecisionHandler is the generic bound decision handler for
// approve/deny approval categories (content_inspection in C1; tool_approval /
// info_leakage reuse it in C2). It maps the clicked action id onto an
// interaction outcome; standing was already enforced by the decision pipe
// (HandleInteractionDecision's DeciderPolicy switch) before this runs. It has
// NO side effect — the runner resumes its gate synchronously off the
// resulting interaction_applied (via the resume bridge); channelsd writes no
// grant here. content_inspection's approve is a pure allow/deny, unlike
// tool_approval's grant tuple, which a category needing that layers on with
// its own distinct handler instead of this one.
func ApprovalDecisionHandler(_ context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
	switch d.Payload.ActionID {
	case "approve":
		return channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil
	case "deny":
		return channelinteractions.Outcome{Result: channelevents.OutcomeDenied}, nil
	default:
		return channelinteractions.Outcome{}, fmt.Errorf("approval decision: unknown actionID %q (category %s)", d.Payload.ActionID, d.Payload.Category)
	}
}
