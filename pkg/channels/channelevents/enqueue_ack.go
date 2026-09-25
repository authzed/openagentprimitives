package channelevents

// EnqueueAckPayload is the body of a KindEnqueueAck envelope, published
// channelsd→channel when an inbound message arrives mid-turn and is queued
// rather than interrupting the agent's in-flight work. The channel kind's
// queued_messages sub-channel sender renders it as a proactive ephemeral
// ack ("your message is queued while I'm working"), alongside an interrupt
// button correlated by RequestID.
type EnqueueAckPayload struct {
	// RequestID is minted by channelsd and correlates this ack with the
	// interrupt button it renders — the same ID rides through
	// InterruptRequestPayload.RequestID when the user clicks it, and back
	// again on InterruptAppliedPayload.RequestID.
	RequestID string `json:"requestID"`
	// Requester is the ephemeral recipient of this ack — the user whose
	// message was queued, not necessarily the session's original starter.
	Requester ExternalIdentity `json:"requester"`
	// SessionRef is "<ns>/<name>" for the busy session — denormalized for the
	// sender's copy; the subject is the routing authority.
	SessionRef string `json:"sessionRef"`
	// Caption is optional "what I'm doing" text describing the agent's
	// in-flight work at the time the message was queued. May be empty when
	// no caption is available.
	Caption string `json:"caption,omitempty"`
}
