// Package subjects owns the NATS subject grammar: every subject any AP
// component publishes on or subscribes to is spelled here, once, and both its
// concrete and its wildcard form derive from that one spelling.
//
// # The grammar
//
//	ap.session.<ns>.<name>.in.<leaf…>                 per-session, toward the runner
//	ap.session.<ns>.<name>.out.<leaf…>                per-session, away from the runner
//	ap.session.<ns>.<name>.history.request            per-session request/reply
//	ap.session.<ns>.<name>.channel_history.request    per-session request/reply
//	ap.monitoring.events                              cluster-wide fanout
//	ap.channel.session_attached                       cluster-wide fanout
//	ap.revocation                                     cluster-wide fanout
//
// <ns> and <name> are the AgentSession's namespace and name. Both are
// Kubernetes DNS labels, so neither can contain a dot and each is exactly one
// NATS token — which is what lets a cluster-wide subscriber wildcard them
// independently. The <leaf> of an in/out subject is a
// channelevents.Kind and MAY be dotted (KindAssistantStreamDelta is
// "assistant.stream.delta"), so it is one-or-MORE tokens and every parser here
// rejoins the tail rather than taking a single token.
//
// # Prefix is the unit callers hold
//
// AgentSession.status.natsSubjectPrefix (and the ChannelBinding field mirroring
// it) stores exactly "ap.session.<ns>.<name>" — a [Prefix]. A caller handed that
// string adopts it with [PrefixOf]; a caller that knows the session builds it
// with [Session]. Both reach every leaf through the same methods, so there is
// one grammar rather than one per source of the prefix.
//
// # Wildcards
//
// A cluster-wide subscriber wildcards the two session tokens with [AnySession]
// (or the equivalent [AnySessionPrefix]) and composes the SAME leaf method its
// publisher calls, so a subscription pattern and the subject it must match
// cannot drift: renaming a leaf renames both, and the compiler finds every site.
//
// # Not here
//
// Request/reply inbox subjects live under the "_INBOX" root, not "ap", and are
// built by apnats.InboxPrefixFor / apnats.InboxSubjectFor (../mint.go). That
// grammar is single-sourced there because it is bound to grant minting and the
// client's CustomInboxPrefix, both of which need the NATS client this package
// deliberately does not import.
//
// This package imports nothing outside the standard library, which is load
// bearing: consumers like the AgentSession controller's grant builder and
// pkg/channels/channelevents must not take a NATS client, a JWT library or an
// nkeys dependency just to spell a subject.
package subjects

import "strings"

// Root is the first token of every subject in this grammar.
const Root = "ap"

// Segment and leaf names. Each appears EXACTLY ONCE in this file; every
// concrete subject, every wildcard subject and every parser below is built
// from these, so a rename here is a rename everywhere or a compile error.
const (
	// sessionSegment marks the per-session subject tree.
	sessionSegment = "session"
	// inSegment carries traffic toward a session's runner.
	inSegment = "in"
	// outSegment carries traffic away from a session's runner.
	outSegment = "out"
	// historyLeaf is the thread-history request/reply leaf (two tokens).
	historyLeaf = "history.request"
	// channelHistoryLeaf is the whole-channel history request/reply leaf
	// (two tokens). Deliberately distinct from historyLeaf so the two paths
	// have separate responders and queue groups.
	channelHistoryLeaf = "channel_history.request"
)

// NATS wildcard tokens. Any matches exactly one token; Tail matches one or
// more and is only legal as the final token of a subject.
const (
	Any  = "*"
	Tail = ">"
)

// AllTree is every subject in this grammar — "ap.>". Only a component trusted
// on the whole bus (channelsd, the CLI) is granted it; a narrower principal
// gets the specific subjects it needs, because an omitted PubAllow entry is
// publish-anywhere, not deny-all.
const AllTree = Root + "." + Tail

// Cluster-wide subjects. Each is fixed — no session tokens — because its
// traffic has no per-AgentSession correlate.
const (
	// Monitoring carries framework health reports (a credential that failed
	// to rotate, a controller that cannot reconcile). Published by the
	// operator's monitoring watcher and by channelsd's pipeline; consumed by
	// channelsd's monitoring relay, which fans each out to every
	// role=monitoring Channel.
	Monitoring = Root + ".monitoring.events"

	// SessionAttached announces that an AgentSession has claimed a Channel as
	// its OutputChannel. Published by channelsd's outbound relay after it
	// patches the session's outputChannel metadata; consumed by channel
	// listeners that keep per-session in-process state.
	SessionAttached = Root + ".channel.session_attached"

	// Revocation carries KindRevoked envelopes that invalidate a credential
	// or tool origin in flight. Published by the operator; consumed by every
	// runner and by the operator's own subscriber.
	Revocation = Root + ".revocation"
)

// AnySessionPrefix is the session prefix with both session tokens wildcarded —
// "ap.session.*.*" — the prefix a CLUSTER-WIDE subscriber builds its pattern
// from. Available as a constant so a subscription can be declared at package
// scope; [AnySession] is the same value typed as a [Prefix], for composing
// leaves through the same methods a publisher uses.
//
// A subscriber that uses it MUST re-derive the session from each message's own
// subject ([ParseIn], [ParseOut], [ParseHistory], [ParseChannelHistory]): the
// subject is the identity NATS authorized, and an envelope body is
// publisher-controlled JSON.
const AnySessionPrefix = Root + "." + sessionSegment + "." + Any + "." + Any

// Cluster-wide per-session subscription patterns, each derived from
// [AnySessionPrefix] and the same leaf name its publisher uses.
const (
	// AnyOutTree matches every outbound subject of every session —
	// "ap.session.*.*.out.>". The subscription channelsd's outbound relay,
	// admind's aggregator and the CLI's local host all hold.
	//
	// The wildcards are two separate tokens rather than one "ap.session.>":
	// "ap.session.>.out.>" is not valid NATS ('>' must be the final token),
	// and the leaf must stay matchable so a consumer can route on it.
	AnyOutTree = AnySessionPrefix + "." + outSegment + "." + Tail

	// AnyHistory matches every session's thread-history request —
	// "ap.session.*.*.history.request". The subscription channelsd's history
	// responder holds; its publisher is Prefix.History.
	AnyHistory = AnySessionPrefix + "." + historyLeaf

	// AnyChannelHistory matches every session's whole-channel history
	// request — "ap.session.*.*.channel_history.request". The subscription
	// channelsd's channel-history responder holds; its publisher is
	// Prefix.ChannelHistory.
	AnyChannelHistory = AnySessionPrefix + "." + channelHistoryLeaf
)

// Prefix is the shared head of every subject concerning ONE AgentSession —
// "ap.session.<ns>.<name>". It is the value AgentSession.status.
// natsSubjectPrefix stores and the unit every per-session leaf hangs off.
type Prefix string

// Session returns the [Prefix] for one AgentSession.
func Session(ns, name string) Prefix {
	return Prefix(Root + "." + sessionSegment + "." + ns + "." + name)
}

// AnySession returns [AnySessionPrefix] as a Prefix, so a cluster-wide
// subscriber composes its pattern through the same leaf methods a publisher
// uses: AnySession().In(leaf) is the pattern matching Session(ns, name).In(leaf)
// for every session.
func AnySession() Prefix { return Prefix(AnySessionPrefix) }

// PrefixOf adopts a prefix a caller was handed rather than built — an
// AgentSession's status.natsSubjectPrefix, or the ChannelBinding field
// mirroring it — so it reaches its leaves through this grammar instead of
// concatenating one.
func PrefixOf(s string) Prefix { return Prefix(s) }

// String returns the prefix itself, for the call sites that store or log it.
func (p Prefix) String() string { return string(p) }

// In returns "<prefix>.in.<leaf>" — the subject channelsd (and the browser
// surfaces webd fronts) publish on, and the session's runner subscribes to.
func (p Prefix) In(leaf string) string {
	return string(p) + "." + inSegment + "." + leaf
}

// Out returns "<prefix>.out.<leaf>" — the subject the session's runner
// publishes on, and channelsd's outbound relay (plus admind and the chat
// surfaces) subscribes to.
func (p Prefix) Out(leaf string) string {
	return string(p) + "." + outSegment + "." + leaf
}

// Tree returns "<prefix>.>" — every subject of one session, in either
// direction. This is the subtree a runner's per-session NATS grant covers on
// SUBSCRIBE, which is why a consumer of any cluster-wide pattern must treat the
// subject, not the envelope body, as the session's identity.
func (p Prefix) Tree() string { return string(p) + "." + Tail }

// OutTree returns "<prefix>.out.>" — every outbound subject of one session.
// This is the subtree a runner's per-session NATS grant covers on PUBLISH.
func (p Prefix) OutTree() string {
	return string(p) + "." + outSegment + "." + Tail
}

// History returns "<prefix>.history.request" — the request/reply subject the
// read_thread_history tool asks on and channelsd's history responder answers.
// Scoped per session so the responder derives the session from the subject
// rather than from a caller-supplied payload field.
func (p Prefix) History() string {
	return string(p) + "." + historyLeaf
}

// ChannelHistory returns "<prefix>.channel_history.request" — the
// request/reply subject the read_channel_history tool asks on and channelsd's
// channel-history responder answers.
func (p Prefix) ChannelHistory() string {
	return string(p) + "." + channelHistoryLeaf
}

// ParseIn extracts the session and leaf from an inbound subject —
// "ap.session.<ns>.<name>.in.<leaf…>", the inverse of Prefix.In.
//
// It exists so a consumer of a cluster-wide inbound pattern routes on the
// identity NATS actually authorized (the subject a publisher's per-session JWT
// let it publish on) rather than on an envelope's publisher-controlled JSON. It
// matters most on this side: a runner's per-session grant covers its whole
// Prefix.Tree, ".in." included, so a publisher authorized on ONE session's
// inbound subject that was trusted on the envelope body could drive channelsd's
// inbound handlers against a DIFFERENT session.
func ParseIn(subject string) (ns, name, leaf string, ok bool) {
	return parseSessionLeaf(subject, inSegment)
}

// ParseOut extracts the session and leaf from an outbound subject —
// "ap.session.<ns>.<name>.out.<leaf…>", the inverse of Prefix.Out. Same
// subject-is-the-authority reasoning as [ParseIn].
//
// The leaf is the REJOINED tail, so the one dotted Kind
// ("assistant.stream.delta") round-trips instead of being silently renamed to
// its first token.
func ParseOut(subject string) (ns, name, leaf string, ok bool) {
	return parseSessionLeaf(subject, outSegment)
}

// ParseHistory extracts the session from a thread-history request subject —
// "ap.session.<ns>.<name>.history.request", the inverse of Prefix.History.
// The responder uses it to key the request to a session without trusting the
// request payload.
func ParseHistory(subject string) (ns, name string, ok bool) {
	return parseSessionFixedLeaf(subject, historyLeaf)
}

// ParseChannelHistory extracts the session from a channel-history request
// subject — "ap.session.<ns>.<name>.channel_history.request", the inverse of
// Prefix.ChannelHistory.
func ParseChannelHistory(subject string) (ns, name string, ok bool) {
	return parseSessionFixedLeaf(subject, channelHistoryLeaf)
}

// parseSessionLeaf is the shared body of [ParseIn] and [ParseOut]. Factored so
// the two directions cannot drift apart in strictness or in their empty-token
// handling; the only difference between them is which segment name they demand.
//
// Six-or-MORE tokens, not exactly six: a leaf may be dotted, and requiring six
// would drop every "assistant.stream.delta" subject.
func parseSessionLeaf(subject, segment string) (ns, name, leaf string, ok bool) {
	p := strings.Split(subject, ".")
	if len(p) < 6 || p[0] != Root || p[1] != sessionSegment || p[4] != segment {
		return "", "", "", false
	}
	if p[2] == "" || p[3] == "" {
		return "", "", "", false
	}
	return p[2], p[3], strings.Join(p[5:], "."), true
}

// parseSessionFixedLeaf is the shared body of [ParseHistory] and
// [ParseChannelHistory]: a per-session subject whose leaf is a fixed, known
// string rather than an open Kind. The leaf may itself be multi-token, so it is
// compared rejoined against the one constant its builder uses.
//
// Empty ns/name tokens are refused for the same reason parseSessionLeaf refuses
// them: "ap.session...history.request" would otherwise resolve to an ObjectKey
// no Get can satisfy, and a responder would answer for a session that cannot
// exist.
func parseSessionFixedLeaf(subject, leaf string) (ns, name string, ok bool) {
	p := strings.Split(subject, ".")
	head := strings.Count(leaf, ".") + 1
	if len(p) != 4+head || p[0] != Root || p[1] != sessionSegment {
		return "", "", false
	}
	if p[2] == "" || p[3] == "" {
		return "", "", false
	}
	if strings.Join(p[4:], ".") != leaf {
		return "", "", false
	}
	return p[2], p[3], true
}
