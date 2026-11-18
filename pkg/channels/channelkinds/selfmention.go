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
