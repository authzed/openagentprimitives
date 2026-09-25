package channelkinds

// SessionCounterparty is implemented by channel kinds whose
// Channel.spec.authzSubject may name another AgentSession in this cluster
// ("agentsession:<namespace>/<name>") as the channel's counterparty, in
// addition to the "service:<id>" every kind accepts. Optional, discovered by
// type assertion.
//
// Not implementing it means "no session counterparty" — the safe default.
// The prefix is what the channelsd pipeline gates on when it decides whether
// an inbound arriving over a Channel of this kind may act as a SESSION rather
// than as a person or a service; admitting that for a kind whose counterparty
// is not a session would let its traffic act with a session's standing
// without that session having spoken at all.
type SessionCounterparty interface {
	AllowsSessionCounterparty() bool
}
