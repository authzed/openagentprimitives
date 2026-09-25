package channelevents

// KindInteractionDecisionRejected is published channelsd→surface (OUT, per
// clicker) when a decision is refused: the clicker lacked standing, the prompt
// was already resolved (spectator), or the bound handler failed (e.g. a
// tool_approval grant-write error). The pipe NEVER swallows a rejected click;
// the surface renders this to the clicker who made it.
const KindInteractionDecisionRejected Kind = "interaction_decision_rejected"

// InteractionDecisionRejectedPayload carries the rejection.
type InteractionDecisionRejectedPayload struct {
	// AgentSessionRef and RequestRef identify the prompt whose click bounced;
	// Category is its channelinteractions category key.
	AgentSessionRef SessionRef `json:"agentSessionRef"`
	Category        string     `json:"category"`
	RequestRef      string     `json:"requestRef"`
	// Clicker is who clicked — the single addressee of this rejection.
	Clicker ExternalIdentity `json:"clicker"`
	// ResponseRef is the surface-opaque handle for editing the clicker's own
	// artifact (Slack: response_url), round-tripped from the decision.
	ResponseRef string `json:"responseRef,omitempty"`
	// Class is machine-readable: "not_authorized" | "already_resolved" |
	// "handler_error" | "category_mismatch".
	Class  string `json:"class"`
	Reason string `json:"reason,omitempty"`
	// OriginalDecider / OriginalOutcome are set for the already_resolved
	// spectator path so the surface can show who actually resolved it.
	OriginalDecider *ExternalIdentity `json:"originalDecider,omitempty"`
	OriginalOutcome string            `json:"originalOutcome,omitempty"`
}
