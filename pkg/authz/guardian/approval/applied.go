package approval

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// FromApplied maps a generic interaction_applied payload onto the Decision a
// blocked runner gate is waiting for, dispatching on the category's REGISTERED
// resume policy. The bool is false when nothing should be delivered — either
// the category declares ResumeNone, or it is not registered in this binary.
//
// This exists as ONE function because it used to exist as two switches over
// category names: cmd/runner's handleInteractionAppliedMessage and the e2e
// harness's subscribeFactoryInteractionApplied, the latter carrying a comment
// promising it was "kept in lockstep" with the former. It was not. The harness
// copy never grew the tool_approval or info_leakage cases, and when the plan
// gate registered plan_phase / plan_amendment neither copy grew those — so an
// approved plan published its prompt, took the click, and then blocked until
// its approval timeout. Both call sites now dispatch off the registry, so a new
// category is wired by declaring Resume on its registration and nowhere else.
//
// registered reports whether the category was found at all, so a caller can log
// the difference between "declared as non-resuming" and "this binary has never
// heard of it" — the second is a wiring bug whose only symptom is a hang.
func FromApplied(pl channelevents.InteractionAppliedPayload) (d Decision, deliver bool, registered bool) {
	cat, ok := channelinteractions.Get(pl.Category)
	if !ok {
		return Decision{}, false, false
	}

	// The person, kept WHOLE. Flattening DecidedBy to its external id here and
	// reconstructing an identity from that string downstream is what put a raw
	// email in front of SpiceDB; the structured identity is right here and is
	// the only thing that canonicalizes correctly for every channel.
	var approverID string
	var approver identity.Principal
	if pl.DecidedBy != nil {
		approverID = pl.DecidedBy.ExternalID.String()
		approver = pl.DecidedBy.Principal()
	}

	switch cat.Resume {
	case channelinteractions.ResumeChoice:
		// InteractionAppliedPayload has no Action field: the N-way answer rides
		// in OutcomeText, which the gate switches on. Approved/denied alone
		// cannot express "which one".
		return Decision{
			Action:     pl.OutcomeText,
			ApproverID: approverID,
			Approver:   approver,
			Reason:     pl.Reason,
		}, true, true

	case channelinteractions.ResumeApproval:
		// Denied and expired both resume as false; only an explicit approval is
		// true. A client-side timeout that already forgot the request makes the
		// false a no-op.
		//
		// ResponseText rides the SAME arm as the approve/deny boolean because the
		// code path IS an approval (login_human_step kind:code): the human both
		// approves AND supplies the free-text code. It is distinct from the
		// ResumeChoice arm's OutcomeText, which carries a CLOSED action id, not a
		// typed string. Empty for every confirm/tool/content/plan approval, so
		// threading it here is a no-op for them. SECURITY: consumed only by the
		// runner's human-step handler, never surfaced to the model.
		return Decision{
			Approved:     pl.Outcome == channelevents.OutcomeApproved,
			ApproverID:   approverID,
			Approver:     approver,
			Reason:       pl.Reason,
			ResponseText: pl.ResponseText,
		}, true, true

	default:
		return Decision{}, false, true
	}
}
