// Package channelfeatures is the kind-agnostic vocabulary connecting an
// agent's CAPABILITIES to the transport PERMISSIONS its channel needs.
//
// It exists to break a dependency direction: pkg/agent/tool/meta/capability
// imports pkg/channels/channelkinds, so channelkinds cannot import it back —
// yet the Slack listener must answer "which scopes does this agent need?" from
// the bound AgentClass. Both sides import this leaf instead.
//
// The capability→feature table is STATIC rather than registered by the
// capability package, because internal/cmd/channelsd — the binary that computes
// ScopesValid — does not import the capability registry at all, so a
// registration design would leave the table silently empty exactly where it
// matters. A mirror test in pkg/agent/tool/meta/capability (the only package
// importing both sides) keeps the table honest.
package channelfeatures

// Feature is one thing an agent can do over a channel, named independently of
// any transport. A channel kind maps each Feature it supports to its own
// permissions; a capability maps to the Features it needs.
type Feature string

const (
	// AttachmentsOutbound: the agent can attach artifacts to its replies.
	AttachmentsOutbound Feature = "attachments.outbound"
	// AttachmentsInbound: the agent can read files the user attaches.
	AttachmentsInbound Feature = "attachments.inbound"
	// ThreadHistory: the agent can read the conversation it is part of.
	ThreadHistory Feature = "history.thread"
	// ChannelHistory: the agent can read the wider channel, not just its thread.
	ChannelHistory Feature = "history.channel"
	// UserLookup: the agent can resolve a name/email to a mentionable user.
	UserLookup Feature = "user.lookup"
	// MessageDelivery: the transport can post and receive messages at all —
	// the agent can speak. Kept separate from StatusIndicator so a degradation
	// message stays honest: losing MessageDelivery is a total outage, losing
	// StatusIndicator is cosmetic (no spinner, replies still land).
	MessageDelivery Feature = "message.delivery"
	// StatusIndicator: the transport can show a native "is thinking…" state.
	StatusIndicator Feature = "status.indicator"
	// CredentialPortal: the transport can host the self-service credential UI.
	CredentialPortal Feature = "credential.portal"
	// SessionViews: the agent can hand the user a link to a session view.
	SessionViews Feature = "session.views"
	// DirectorySync: the app's own bot token can enumerate this workspace's
	// channels and their membership. Unlike every other Feature here, this
	// is not something the BOUND AGENT does — it is what makes the SAME
	// token usable as a RelationshipSource's spec.auth credential
	// (pkg/platform/relsync, pkg/controllers/relationshipsource), so a
	// workspace directory sync has a token to read with at all. Opt-in and
	// off by default on purpose: granting it to every per-agent app would
	// multiply the blast radius of a workspace-wide directory read by the
	// number of agents — any app may signal, but exactly one credential
	// fetches — check it only on the one app an operator actually intends
	// to use for the sync.
	DirectorySync Feature = "directory.sync"
)

// All returns every Feature in declaration order.
func All() []Feature {
	return []Feature{
		AttachmentsOutbound, AttachmentsInbound,
		ThreadHistory, ChannelHistory,
		UserLookup, MessageDelivery, StatusIndicator,
		CredentialPortal, SessionViews, DirectorySync,
	}
}
