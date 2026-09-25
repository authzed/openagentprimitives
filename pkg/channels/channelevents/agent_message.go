package channelevents

// KindAgentMessageSend is the one envelope a session-to-session message
// travels in. It is published by the SENDING AgentSession on its OWN inbound
// subject — ap.session.<ns>.<name>.in.agent_message_send — to ask channelsd to
// deliver a message to another session, named in the payload. channelsd
// consumes it centrally (internal/cmd/channelsd/main.go's
// "agent_message_send" row) and delivers through
// pkg/channels/channelsd/pipeline.Pipeline.HandleAgentMessageSend: the one
// Channel joining the pair, the kind's own session-counterparty gate, and an
// agentsession#converse check.
//
// # Why the subject names the SENDER
//
// Every inbound subscription is the cluster-wide "ap.session.*.*.in.<kind>",
// and channelsd cross-checks Envelope.Session against the subject the
// publisher was authorized on (inboundSubjectAuthorized). So whichever end the
// subject names is the end the bus authenticated; the other end is a claim the
// consumer has to corroborate. Naming the sender puts the authentication where
// the authorization question is — WHO is speaking — and leaves the
// destination as the claim, which pipeline.resolvePairChannel corroborates
// against a real, K8s-witnessed Channel joining the two before anything is
// carried. No Channel joining them, no delivery.
//
// It is also the only subject a RUNNER can publish on. Its per-session NATS
// grant enumerates publish rights on its own prefix only (runnerNATSUserGrant
// in pkg/controllers/agentsession/controller.go), and widening it to another
// session's inbound subtree would let any runner inject inbound traffic into
// any session in the cluster — the opposite of what the grant exists to
// prevent.
//
// Both producers therefore publish exactly this: reply_to_subagent
// (pkg/agent/tool/meta) from a runner, and the `agent` channel kind's Sender
// (pkg/channels/channelkinds/agent) from inside channelsd. The Sender could
// address the counterparty's subject instead — it holds a cluster-wide
// credential — and deliberately does not, because that direction would carry
// the sender as an unauthenticated payload field.
//
// # Why it carries content
//
// Unlike `.in.user_message`'s content-free wake-up shape, this envelope holds
// the message text — the same durable reason KindViewMessage does. For every
// kind with a listener, the listener already holds the content (it read it off
// a socket, an HTTP webhook, a Bento message) and only needs to WAKE the
// session, because memory is the durable record. The agent kind's transport IS
// the bus, and it has no Listener of its own to be that consumer
// (channelkinds.Deps grants no subscribe capability), so there is no third
// place to hold the content and the envelope has to.
const KindAgentMessageSend Kind = "agent_message_send"

// AgentMessageSendPayload is the body of a KindAgentMessageSend envelope.
//
// There is deliberately no `from` field, and there must never be one: the
// sender is the session the SUBJECT authorized, and a payload field claiming
// it would be a second, weaker answer to the same question.
type AgentMessageSendPayload struct {
	// To is the destination AgentSession. A CLAIM, corroborated by channelsd
	// against a real Channel joining it to the sender (the session the
	// subject authorized) before anything is carried.
	To SessionRef `json:"to"`
	// Text is the message body.
	Text string `json:"text"`
}
