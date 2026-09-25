package categories

// identityChoiceOutcomeLabels maps identity_choice's 3-way ActionID (see
// pkg/agent/runner/identitygate.go) to the friendly text a renderer shows for a
// RESOLVED prompt.
//
// The ActionID rides the wire verbatim in InteractionAppliedPayload.OutcomeText:
// channelsd's IdentityChoiceDecisionHandler sets it, and internal/cmd/runner's
// subscribeInteractionApplied reads it straight back out as
// approval.Decision.Action, which is what IdentityChoiceGate.Eval switches on.
// OutcomeText is therefore load-bearing on the wire and must NEVER be changed
// to a friendly label at the source; this mapping is presentation-only, applied
// at render time.
//
// OutcomeText is also category-overloaded — credential_link carries a credential
// name in it — so callers MUST gate on Category == IdentityChoice first.
var identityChoiceOutcomeLabels = map[string]string{
	"agent":           "Running as the agent",
	"userPassthrough": "Running as you",
	"cancel":          "Cancelled",
}

// IdentityChoiceOutcomeLabel returns the friendly label for one of
// identity_choice's 3-way ActionIDs, or ok=false for anything else (an
// unrecognized action id). Callers are responsible for gating on
// Category == IdentityChoice first — see identityChoiceOutcomeLabels' doc.
func IdentityChoiceOutcomeLabel(actionID string) (label string, ok bool) {
	label, ok = identityChoiceOutcomeLabels[actionID]
	return label, ok
}
