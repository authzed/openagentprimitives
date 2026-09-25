// pkg/channels/channelevents/identity_choice.go
//
// The per-kind identity-choice envelope shapes. NOTE: nothing publishes or
// consumes these three kinds today — the live identity-choice gate rides the
// generic interaction model (KindInteractionRequest / …Applied / …Decision)
// under category "identity_choice", whose 3-way answer travels in
// InteractionAppliedPayload.OutcomeText.
package channelevents

// IdentityChoiceRequestPayload is the body of a KindIdentityChoiceRequest
// envelope, published runner→channelsd. Rendered as a 3-button ephemeral to
// Requester.
type IdentityChoiceRequestPayload struct {
	// RequestID correlates this prompt with the decision and applied echoes.
	RequestID        string           `json:"requestID"`
	Requester        ExternalIdentity `json:"requester"`             // WHO is asked (initiating user)
	AgentDisplayName string           `json:"agentDisplayName"`      // "Run as <this>"
	Mode             string           `json:"mode"`                  // "ask" | "dynamic"
	Recommended      string           `json:"recommended,omitempty"` // "" | "agent" | "userPassthrough"
	Reason           string           `json:"reason,omitempty"`      // advisory suggestion text (dynamic)
	// ChoiceTTL is how long the runner will wait, as a Go duration string;
	// surfaces render it so the user knows the prompt expires.
	ChoiceTTL string `json:"choiceTTL"`
}

// IdentityChoiceAppliedPayload is published channelsd→runner. Carries the
// resolved 3-way answer via Action.
type IdentityChoiceAppliedPayload struct {
	RequestID  string `json:"requestID"`
	Action     string `json:"action"`           // "agent" | "userPassthrough" | "cancel"
	ApproverID string `json:"approverID"`       // channel-native user id of the clicker
	Reason     string `json:"reason,omitempty"` // "timeout" on timeout-applied
	// Denormalized so a surface can re-render the resolved prompt without
	// holding the original request across a restart.
	AgentDisplayName string `json:"agentDisplayName,omitempty"`
	ResponseURL      string `json:"responseURL,omitempty"`
}

// IdentityChoiceDecisionPayload is published by the channel kind's listener
// on button click (slack-listener→channelsd).
type IdentityChoiceDecisionPayload struct {
	RequestID string `json:"requestID"`
	// Approver is the clicker as the surface saw them — a CLAIM about who
	// pressed the button, never an authorization input on its own.
	Approver    ExternalIdentity `json:"approver"`
	Action      string           `json:"action"` // "agent" | "userPassthrough" | "cancel"
	ResponseURL string           `json:"responseURL,omitempty"`
}
