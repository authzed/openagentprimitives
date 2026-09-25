// Package agent is the channel kind for a session-to-session conversation:
// the counterparty is another AgentSession in the same cluster, not a
// third-party transport. A `task` or `chat` mode subagent (see
// v1alpha1.SubagentRequestSpec.Mode) is bound to a Channel of this kind so it
// has a surface to talk on; a `single_turn` child stays headless and never
// gets one.
//
// Unlike slack (talks to Slack) or bento (talks to Bento), this kind's
// destination is the AP bus itself: delivery is a cluster-side write onto the
// counterparty session's inbound path, not a call to an external API. That is
// why RelayedByChannelsd is true here — the same as every channelsd-hosted
// kind — even though there is no third-party service on the other end.
//
// Every Kind method is implemented here. NewSender delivers for real (see
// sender.go). NewListener returns a documented no-op — see its own doc for why
// this kind can never have a real Listener — and a published
// KindAgentMessageSend has a real consumer: channelsd subscribes centrally
// (internal/cmd/channelsd/main.go's "agent_message_send" row) and delivers
// through HandleAgentMessageSend (pkg/channels/channelsd/pipeline).
//
// Splitting human-directed traffic from conversational traffic is NOT done in
// this package, and cannot be: SubChannelSender receives a Channel and never a
// session (channelkinds.Kind's own signature), so a kind has nothing to
// resolve a lineage against. The split lives in the outbound relay
// (pkg/channels/channelsd/outbound), which has the session in hand and asks
// this kind DeliversToHuman before routing a human-directed envelope.
package agent

import (
	"context"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// KindName is the Channel.spec.kind discriminator for this kind. It is also
// the value stored in the AgentSession's channel-kind label and in
// spec.inputChannel.kind. Exported the same way browser's and local's are, so
// the SubagentRequest controller — which provisions a Channel of this kind for
// a conversational subagent — names the kind from here instead of repeating a
// string literal it cannot be kept in step with.
const KindName = "agent"

func init() { registry.Register(Kind{}) }

// Kind implements channelkinds.Kind for session-to-session channels. It is
// stateless — a Channel's counterparty is fully described by
// Channel.spec.authzSubject (an "agentsession:<ns>/<name>" value), so nothing
// needs to be carried on the Kind value itself.
type Kind struct{}

// Compile-time interface checks.
var (
	_ channelkinds.Kind                = Kind{}
	_ channelkinds.SessionCounterparty = Kind{}
)

func (Kind) Name() string { return KindName }

// DefaultSessionScope is "singleton": an agent Channel is dedicated to one
// counterparty session for its whole lifetime — there is no per-user or
// per-thread axis to correlate inbound messages against, unlike a durable
// channel that hosts many independent conversations.
func (Kind) DefaultSessionScope() string { return "singleton" }

// Capabilities: text only. The counterparty is another session, not a
// rendering surface — a turn crosses this channel as plain text, with no
// markdown dialect or asset MIME to negotiate.
func (Kind) Capabilities() []string { return []string{"text"} }

// NewListener returns an inert placeholder, and always will:
// channelkinds.Deps grants a kind only Deps.NATSPublish (one-off) and
// Deps.NATSRequest (request-reply) — never a subscribe capability or a raw
// *nats.Conn — so a channel-kind package has no seam through which to
// subscribe to the bus subject this session is addressed on. Real inbound
// surfacing is serviced centrally instead: internal/cmd/channelsd/main.go
// subscribes ap.session.*.*.in.agent_message_send (a wildcard across every
// session, like every other channelsd-hosted inbound kind) and delivers
// through pkg/channels/channelsd/pipeline.Pipeline.HandleAgentMessageSend —
// the same component that already holds the cluster-wide NATS credential and
// the write-capable memory token every other kind's inbound goes through.
func (Kind) NewListener(channelkinds.Deps) channelkinds.Listener { return noopListener{} }

// NewSender returns the real Sender: it publishes onto the SENDING session's
// inbound bus subject, naming the counterparty in the payload, rather than
// calling a third-party API. See sender.go for the delivery mechanism, why it
// uses Deps.NATSPublish rather than Deps.Inbound, and why the sender's subject
// rather than the counterparty's.
func (Kind) NewSender(deps channelkinds.Deps) channelkinds.Sender { return newAgentSender(deps) }

// SubChannelSender returns nil for every name. Every sub-channel this project
// has is a bespoke rendering of something for a person — an interaction card,
// a "here is a link" offer, a thread title, a queued-message note — and the
// counterparty here is a session, which has no rendering surface at all. nil
// is the interface's own documented "this kind doesn't implement that
// sub-channel" answer, so the relay drops those envelopes and logs it rather
// than delivering agent-shaped prose to a session's context.
//
// Human-directed envelopes do not arrive here in the first place: the relay
// resolves them onto an ancestor's human binding (see DeliversToHuman) and
// builds the sub-channel sender against THAT Channel's kind.
func (Kind) SubChannelSender(string, channelkinds.Deps) channelkinds.Sender { return nil }

// NewStreamDeltaSink returns nil: the agent kind opts out of stream-delta
// rendering. Its counterparty is another session, which consumes whole
// turns via the Listener/Sender path, not token-by-token deltas the way a
// human-facing UI does; the relay drops KindAssistantStreamDelta envelopes
// for kinds that opt out, which is what we want here.
func (Kind) NewStreamDeltaSink(channelkinds.Deps) channelkinds.StreamDeltaSink { return nil }

// SupportsLiveViewOffer is false, and SubChannelSender returns nil for
// "live_view_offer" like it does for every other name — the two are one fact,
// per the interface's contract. A live-view offer is a link for a PERSON to
// open in a browser; the counterparty here is another session, which has no
// browser and no rendering surface. Declared rather than derived so
// artifact_offer_view refuses in the runner instead of reporting an offer
// "sent" that the relay then drops.
func (Kind) SupportsLiveViewOffer() bool { return false }

// SupportsMonitoring is false: NewMonitoringSender below returns nil. The two
// are one fact, per the interface's own contract. A session-to-session
// channel is not a place to deliver cluster-level framework events — there is
// no operator on the other end to read them.
func (Kind) SupportsMonitoring() bool { return false }

// NewMonitoringSender returns nil: see SupportsMonitoring.
func (Kind) NewMonitoringSender(channelkinds.Deps) channelkinds.MonitoringSender { return nil }

// SupportedRoles is every role, stated as the full set rather than nil — nil
// means "serves no role", which is not what this kind means. The delegation
// controller creates its per-delegation Channels as role=both (both ends
// speak), and ValidateSpec below gates no role, which is the condition the
// interface attaches to this answer: narrowing the list while ValidateSpec
// stays permissive would make `oap agent lint` reject a Channel the cluster
// then accepts. Narrow it only together with a gate that reads THIS list.
func (Kind) SupportedRoles() []string { return spiceboxv1alpha1.AllChannelRoles() }

// ValidateSpec rejects a Channel that also carries another kind's config
// block, and REQUIRES Channel.spec.authzSubject to name a well-formed
// "agentsession:<ns>/<name>" counterparty (authz.ParseAgentSessionSubject —
// the same parser the Sender uses, reused here rather than re-validated ad
// hoc, so a malformed value fails at admission instead of at first Send).
//
// The field is mandatory because an agent Channel with no counterparty is not
// an agent Channel: it names neither an end to address nor an end whose
// traffic it may carry, and both of this kind's halves are defined by it —
// the Sender names the counterparty as the destination of what it publishes,
// and channelsd resolves an arriving message's Channel from the (target,
// sender) pair by requiring this value to name the OTHER end. On this kind it
// is a pair-resolution input, NOT the acting subject: an arriving message is
// attributed to the session whose NATS subject it was published on, which on a
// child -> parent message is the end this field does not name.
// This is the only check that demands it: the AgentClass controller's
// userLessChannelMissingAuthz rule skips every kind whose
// SpawnsSessionOnInbound is false, which includes this one.
//
// Refused at VALIDATION rather than at first use, per the repo's
// invalid-opted-in-dependency idiom: there is no Channel admission webhook, so
// the refusal is the Channel controller marking the CR invalid
// (ReasonChannelSpecInvalid) on its first reconcile, not an apiserver reject.
// The failure without it is loud but far later — newAgentSender captures the
// parse error and every Send on that channel returns it — which surfaces as a
// conversational child that silently cannot speak, long after the Channel that
// broke it was created.
func (Kind) ValidateSpec(ch *spiceboxv1alpha1.Channel) error {
	if ch == nil {
		return nil
	}
	if ch.Spec.Slack != nil {
		return errSpecForeignBlock("slack")
	}
	if ch.Spec.Fake != nil {
		return errSpecForeignBlock("fake")
	}
	if ch.Spec.Bento != nil {
		return errSpecForeignBlock("bento")
	}
	if _, err := authz.ParseAgentSessionSubject(ch.Spec.AuthzSubject); err != nil {
		return fmt.Errorf("spec.authzSubject: %w", err)
	}
	return nil
}

func errSpecForeignBlock(block string) error {
	return fmt.Errorf(`spec.%s must be empty when kind="agent"`, block)
}

// RequiredSecretKeys: empty. The counterparty is in-cluster; there is no
// third-party credential to hold in a Secret.
func (Kind) RequiredSecretKeys(*spiceboxv1alpha1.Channel) []string { return nil }

// PublicSecretKeys is nil: this kind holds no Secret at all — the counterparty
// is in-cluster — so it can vouch for no key in one. Anything an operator put
// there stays secret.
func (Kind) PublicSecretKeys(*spiceboxv1alpha1.Channel) []string { return nil }

// FeatureSupport reports no transport permissions: this kind has no
// permission model, so every feature it can express is free.
func (Kind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return nil
}

// RenderMention: the agent kind has no @-tag syntax; return the bare id.
func (Kind) RenderMention(externalID string) string { return externalID }

// SupportedMentionLookups: none. There is no human directory to look
// against on a session-to-session channel.
func (Kind) SupportedMentionLookups() []channelkinds.MentionLookupKind { return nil }

// LookupUser always returns ErrMentionUnsupported. Reached only via
// deliberate bypass — the tool itself is omitted from agent-bound sessions
// because SupportedMentionLookups() is empty.
func (Kind) LookupUser(
	context.Context, channelkinds.LookupDeps,
	channelkinds.MentionLookupKind, string,
) (string, string, error) {
	return "", "", channelkinds.ErrMentionUnsupported
}

// MentionToolDescription: empty; no mention tool is registered.
func (Kind) MentionToolDescription() string { return "" }

// TextFormattingInstructions implements channelkinds.TextFormatter. The
// counterparty is another session's context, not a rendering surface —
// nothing on the other end interprets Markdown, HTML, or any other markup,
// so a model that emits `**bold**` or `# heading` puts those characters into
// the counterparty's context literally, not as formatting.
func (Kind) TextFormattingInstructions() string {
	return "Plain text only. The counterparty is another agent session, not a rendering " +
		"surface — nothing interprets Markdown, HTML, or any other markup, so characters " +
		"like ** or # would appear to the counterparty exactly as typed. Write plain sentences."
}

// UserAttributable is false: an agent channel's inbound comes from another
// session, not a human, so it carries no per-user identity. The counterparty
// is attributed via Channel.spec.authzSubject (an "agentsession:" subject),
// which is exactly the mechanism UserAttributable's doc says a
// non-attributable kind must supply.
func (Kind) UserAttributable() bool { return false }

// DeliversToHuman is false, and this kind is the reason the question exists.
// What reads an agent Channel is another AgentSession — a model's context
// window, not a person. A conversational child is bound to one of these as its
// spec.inputChannel, so its own outbound binding IS this Channel; answering
// true would send a permission prompt, a credential request, or an identity
// choice to the PARENT AGENT, which would then be the thing positioned to
// answer a question that only a human has the standing to answer.
//
// Not the same fact as UserAttributable above, which is about inbound identity
// and gates the AgentClass binding validator. Both are false here; they are
// still two facts (see the interface doc).
func (Kind) DeliversToHuman() bool { return false }

// AllowsSessionCounterparty implements channelkinds.SessionCounterparty.
// True: this kind's counterparty IS another AgentSession in this cluster —
// that is its entire reason to exist — so Channel.spec.authzSubject may name
// one as an "agentsession:<namespace>/<name>" value. The channelsd pipeline
// consults this (via the registry, by kind name) to decide which
// authzSubject prefixes a Channel of a given kind may carry; every kind that
// does not implement this interface stays service:-only.
func (Kind) AllowsSessionCounterparty() bool { return true }

// AllowsSyntheticIdentity is false. This gate concerns a view_message inbound
// resolving a human's email-less identity to a synthetic subject; an agent
// channel's inbound is attributed to the SENDING SESSION — the session the
// arriving NATS subject authorized, carried into the pipeline as the inbound's
// AuthzSubject with no external ids at all — never to a per-user claim, so
// the gate this answers never applies here.
func (Kind) AllowsSyntheticIdentity() bool { return false }

// RelayedByChannelsd is true: delivery is a cluster-side write onto the AP
// bus, not a client-side render, so channelsd hosts this kind's
// Listener/Sender like every other channelsd-hosted kind. Do not copy
// browser's or local's false — those are client-hosted kinds whose surface
// lives in a browser or a terminal; this kind has no client at all.
func (Kind) RelayedByChannelsd() bool { return true }

// SpawnsSessionOnInbound is false: an agent Channel is dedicated to one
// counterparty session, pre-created before the Channel is bound to it. An
// inbound with no active session must not spawn a phantom one — the same
// reasoning as the local TUI kind, which also owns exactly one session for
// the lifetime of its transport.
func (Kind) SpawnsSessionOnInbound() bool { return false }

// WebAuthenticator returns nil: an agent Channel binds one session to
// another session, not a third-party service a human authenticates against.
func (Kind) WebAuthenticator(channelkinds.WebAuthDeps) channelkinds.WebAuthenticator { return nil }

// WebhookReceiver returns nil: this kind has no inbound HTTP surface. Both
// ends of an agent Channel are AgentSessions in this cluster and messages
// cross on the bus, so there is nothing outside for webd to receive.
func (Kind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver { return nil }

// Wizard returns UnavailableWizard: an agent Channel binds one session to
// another session, not a third-party service a human configures by hand, so
// there is no interactive `oap channel create --kind agent` flow. The
// interface still requires a non-nil Wizard.
func (Kind) Wizard() channelkinds.Wizard {
	return channelkinds.UnavailableWizard(
		"agent kind has no interactive wizard: it binds one session to another session, not a service a human sets up")
}

// noopListener is the placeholder NewListener returns, permanently — see
// NewListener's doc for why this kind can never own a subscription. Start/Stop
// succeed without doing anything; channelsd still starts and stops this
// Listener like any other kind's (RelayedByChannelsd is true), it is just a
// no-op every time.
type noopListener struct{}

func (noopListener) Start(context.Context) error { return nil }
func (noopListener) Stop(context.Context) error  { return nil }
