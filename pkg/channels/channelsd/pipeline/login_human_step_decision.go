package pipeline

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// BindLoginHumanStepHandler binds the login_human_step decision handler — a
// browser toolbox's request_human_step confirm (kind:confirm) or code
// (kind:code) card.
//
// It is a PURE approve/deny with NO grant-write side effect: unlike tool_approval
// (which writes a grant tuple), deciding a sign-in-step card writes nothing. The
// runner resumes request_human_step's block synchronously off the resulting
// interaction_applied (via approval.FromApplied / the resume bridge). Standing is
// enforced FIRST by the interaction_decision pipe's DecideApprovers policy (the
// clicker must be in the session approve-set) before this runs.
//
// The ONE thing it does beyond the generic ApprovalDecisionHandler is carry the
// person's FREE-TEXT answer through on an approve — the emitted/SMS login code for
// kind:code. The runner types that code into the page element the agent named; the
// model never sees it (see pkg/agent/tool/browser's requestHumanStep). The confirm
// half carries no ResponseText, so the same handler serves both.
func BindLoginHumanStepHandler(_ *Pipeline) {
	channelinteractions.Bind(categories.LoginHumanStep, loginHumanStepDecisionHandler)
}

// loginHumanStepDecisionHandler maps the clicked action onto an approve/deny
// outcome (delegating to ApprovalDecisionHandler) and, on an approve, carries the
// decider's typed code through to the runner on Outcome.ResponseText. On a deny
// the code is dropped — a declined card types nothing.
//
// SECURITY: the code rides ResponseText (a free-text field), not OutcomeText (a
// closed action id) and not any rendered field. The decision pipe copies it onto
// the OUTBOUND applied envelope (the leg the runner resumes off), stripped from the
// IN cleanup copy — no surface renders it, and it never reaches the model or a log.
func loginHumanStepDecisionHandler(ctx context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
	out, err := ApprovalDecisionHandler(ctx, d)
	if err != nil {
		return channelinteractions.Outcome{}, err
	}
	if out.Result == channelevents.OutcomeApproved {
		out.ResponseText = d.Payload.ResponseText
	}
	return out, nil
}
