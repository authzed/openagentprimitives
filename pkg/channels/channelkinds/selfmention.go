package channelkinds

// SelfMentioner is implemented by channel kinds on which the agent itself has
// a mentionable identity — a Slack bot user, say. Optional and discovered by
// type assertion, so callers no-op rather than branching on kind.
//
// It exists because a message that summons the agent usually opens with the
// agent's own mention token. Without being told whose token that is, a model
// reads it as the person it is talking to and pings itself in its reply.
type SelfMentioner interface {
	// SelfMention returns the token that mentions the agent on this channel,
	// derived from the session's input-channel binding external map (the
	// listener stamps the bot's id there). "" when it cannot be derived.
	SelfMention(external map[string]string) string
}

// SenderMentioner is implemented by channel kinds whose users can be
// @-mentioned by a token derived from their external id alone — Slack's
// "<@U…>". Optional and discovered by type assertion, like SelfMentioner.
//
// It is deliberately not Kind.RenderMention: that method falls back to the
// bare id for a kind with no mention syntax, which is right for a notice a
// person reads and wrong for InboundEvent.SenderMention, where a bare id would
// be handed to the agent as though it were a token that pings someone.
//
// A listener stamps InboundEvent.SenderMention itself. This is for the paths
// that rebuild an inbound from a stored identity, after the listener's event
// is gone — a message held for approval and replayed once approved.
type SenderMentioner interface {
	// SenderMention returns the token that mentions the user with this
	// external id on this channel kind. "" when externalID is empty.
	SenderMention(externalID string) string
}
