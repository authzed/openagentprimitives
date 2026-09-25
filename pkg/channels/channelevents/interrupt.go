package channelevents

// InterruptRequestPayload is the body of a KindInterruptRequest envelope,
// published channel→runner when a user requests a mid-turn interrupt of the
// agent's in-flight work.
type InterruptRequestPayload struct {
	// RequestID is the id minted on the enqueue ack whose button was pressed;
	// it rides back on InterruptAppliedPayload.RequestID.
	RequestID string `json:"requestID"`
	// SessionRef is "<ns>/<name>" for the session to interrupt — denormalized
	// for logging; the subject is the routing authority.
	SessionRef string `json:"sessionRef"`
	// Requester is who asked, as the surface saw them — a CLAIM, gated on
	// arrival, never an authorization input on its own.
	Requester ExternalIdentity `json:"requester"`
	// ResponseURL is Slack's response_url captured at button-click time
	// (empty on the web path). It rides through to the applied echo so the
	// Slack applied-handler — a different object than the listener that
	// captured it — can edit the ephemeral in place.
	ResponseURL string `json:"responseURL,omitempty"`
}

// InterruptAppliedPayload is the body of a KindInterruptApplied envelope,
// published runner→channel carrying the outcome of an interrupt request.
type InterruptAppliedPayload struct {
	RequestID string `json:"requestID"`
	// Outcome is "interrupted" | "rejected".
	Outcome string `json:"outcome"`
	// Reason explains a "rejected" outcome in human terms; empty on success.
	Reason string `json:"reason,omitempty"`
	// ResponseURL is copied from the originating InterruptRequestPayload so
	// the Slack applied-handler can edit the ephemeral in place.
	ResponseURL string `json:"responseURL,omitempty"`
}
