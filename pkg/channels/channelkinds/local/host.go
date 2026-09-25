package local

import (
	"context"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/clienthosted"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// HostConfig is the input to NewHost.
type HostConfig struct {
	// Deps is the generic channelkinds.Deps `oap` builds. This kind ignores
	// Secret/AssetFetcher/AuthzReader/watchdog hooks. The three view minters
	// feed their matching *_offer sub-channel senders; a nil one makes that
	// sender fail loudly rather than drop the offer.
	Deps channelkinds.Deps
	// Sink is the bubbletea-program-backed EventSink the senders push
	// render events into. Required.
	Sink EventSink
	// User is the local `oap` operator's identity — the session's
	// started_by. ExternalID should be a stable per-machine id.
	User channelkinds.ExternalIdentity
	// Namespace and SessionName address the AgentSession this host's
	// Listener submits view_message requests to over NATS request-reply
	// (ap.session.<Namespace>.<SessionName>.in.view_message). They are
	// distinct from the Channel's own (namespace, name) — the ephemeral
	// local Channel is named "<session>-chan", not the session name
	// itself — so they cannot be derived from Deps.Channel.
	Namespace   string
	SessionName string
	// Principal is the resolved CLI identity for this chat session. Used by
	// the live_view_offer sub-channel sender to mint a signed webd deep-link
	// scoped to the session user. Optional: when zero-value, the link is
	// minted with an anonymous principal (no subject claim).
	Principal identity.Principal
	// Via is this surface's view URN (viewurn.Format(viewurn.TypeTUI, "",
	// "")) — stamped on every SubmitUserMessage so the pipeline and the
	// agent's context know which surface originated the inbound. Optional:
	// zero-value means no via is attached.
	Via string
}

// Host binds the generic Deps to a concrete EventSink and exposes the
// host-side Listener/Sender objects `oap` runs. It also satisfies
// outbound.SenderResolver — scoped, in both halves, to the ONE session this
// process is chatting with (see owns / Accepts).
//
// The scoping is not an optimization. outbound.Relay subscribes the
// cluster-wide "ap.session.*.*.out.>" and the CLI's NATS grant is "ap.>", so
// this host is handed every session's outbound envelopes. Resolving a sender
// for a foreign one renders another conversation into this TUI, and for an
// interaction_request it is worse than a display leak: the TUI opens its
// BLOCKING decision modal on the foreign payload and submits the answer under
// THIS session's name with the foreign requestRef — a misrouted decision and a
// foreign prompt left unanswered. Both halves are needed; either alone still
// leaves the resolver session-blind.
type Host struct {
	deps channelkinds.Deps
	// sink is the caller's EventSink behind this kind's ONE inerting door
	// (inert_sink.go). Every sender below is built from it, and their sink
	// fields are typed *inertSink, so there is no way to hand one a raw sink.
	sink        *inertSink
	user        channelkinds.ExternalIdentity
	channelKey  string
	principal   identity.Principal
	ns          string
	sessionName string
	via         string
}

// NewHost validates the config and constructs a Host. The channelKey is
// derived from the Channel CR so it is stable across runner-pod
// respawns — the pipeline correlates every inbound to the same
// AgentSession via this key.
func NewHost(cfg HostConfig) (*Host, error) {
	if cfg.Sink == nil {
		return nil, fmt.Errorf("local.NewHost: sink is required")
	}
	if cfg.Deps.Channel == nil {
		return nil, fmt.Errorf("local.NewHost: deps.channel is required")
	}
	// Namespace + SessionName are what every resolver method and Accepts scope
	// to. An unset pair would make this host match nothing, silently dropping
	// its own session's every reply, prompt and stream delta with no error
	// anywhere — so refuse to build it rather than trust a caller to remember.
	if cfg.Namespace == "" || cfg.SessionName == "" {
		return nil, fmt.Errorf("local.NewHost: namespace and sessionName are required — they scope which session this host renders")
	}
	ch := cfg.Deps.Channel
	return &Host{
		deps: cfg.Deps,
		// Wrapped HERE, once: this is the single place every real sender's sink
		// comes from, so it is what makes inertness a property of the kind
		// instead of something each sender file has to remember.
		sink:        newInertSink(cfg.Sink),
		user:        cfg.User,
		channelKey:  ChannelKey(ch.Namespace, ch.Name),
		principal:   cfg.Principal,
		ns:          cfg.Namespace,
		sessionName: cfg.SessionName,
		via:         cfg.Via,
	}, nil
}

// ChannelKey returns the stable per-session correlation key for a local
// Channel. Exported so `oap agent chat` can stamp the matching
// sha256-hashed label on the AgentSession it pre-creates (the pipeline
// correlates inbound→session via channel.name + sha256(channelKey)).
func ChannelKey(ns, channelName string) string {
	return "local:" + ns + "/" + channelName
}

// Listener returns the host-side Listener the TUI drives.
func (h *Host) Listener() *localListener {
	return &localListener{Listener: clienthosted.Listener{
		Deps:        h.deps,
		Ext:         h.user,
		ChannelKey:  h.channelKey,
		Namespace:   h.ns,
		SessionName: h.sessionName,
		Via:         h.via,
		Kind:        KindName,
	}}
}

// Accepts is the relay's pre-Get scope (outbound.Relay.Accept): it reports
// whether an outbound envelope for (ns, name) could possibly belong to the
// session this host serves. It answers from the same one pair of fields owns
// does, one step earlier — before the AgentSession is loaded — so the two can
// never disagree, and a foreign envelope costs no API round-trip on the single
// goroutine the subscription's callbacks are serialized on.
func (h *Host) Accepts(ns, name string) bool {
	return ns == h.ns && name == h.sessionName
}

// owns reports whether sess is the one session this host renders. A nil
// session is fail-closed: the relay only ever passes the AgentSession it
// loaded, so nil means the envelope's owner could not be established.
func (h *Host) owns(sess *spiceboxv1alpha1.AgentSession) bool {
	return sess != nil && h.Accepts(sess.Namespace, sess.Name)
}

// SenderFor implements outbound.SenderResolver — returns the main
// host-side Sender for this host's own session, and (nil, nil) for any
// other, which tells outbound.Relay to drop the envelope.
func (h *Host) SenderFor(_ context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error) {
	if !h.owns(sess) {
		return nil, nil
	}
	return &localSender{sink: h.sink}, nil
}

// SubChannelSenderFor implements outbound.SenderResolver — returns the
// host-side sub-channel sender for the named sub-channel, nil for
// unknown names and for any session but this host's own.
func (h *Host) SubChannelSenderFor(_ context.Context, sess *spiceboxv1alpha1.AgentSession, name string) (channelkinds.Sender, error) {
	if !h.owns(sess) {
		return nil, nil
	}
	switch name {
	case "message":
		return &localSender{sink: h.sink}, nil
	case "tool_session":
		return &toolSessionSender{sink: h.sink}, nil
	case "permission_request":
		return &permissionRequestSender{sink: h.sink}, nil
	case "queued_messages":
		return &queuedMessagesSender{sink: h.sink}, nil
	case "live_view_offer":
		return &liveViewOfferSender{
			sink:      h.sink,
			minter:    h.deps.ArtifactViewMinter,
			principal: h.principal,
		}, nil
	case "session_view_offer":
		return &sessionViewOfferSender{
			sink:      h.sink,
			minter:    h.deps.SessionViewMinter,
			principal: h.principal,
		}, nil
	case channelkinds.SubChannelAgentUIOffer:
		// No principal: AgentUIMinter takes only the sessionRef.
		return &agentUIOfferSender{
			sink:   h.sink,
			minter: h.deps.AgentUIMinter,
		}, nil
	case "interaction":
		return &interactionSender{sink: h.sink}, nil
	default:
		return nil, nil
	}
}

// StreamDeltaSinkFor implements outbound.SenderResolver — returns the
// host-side StreamDeltaSink, or nil for a session other than this host's own,
// or when the session's AgentClass has not opted into showAssistantStream.
func (h *Host) StreamDeltaSinkFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.StreamDeltaSink, error) {
	if !h.owns(sess) {
		return nil, nil
	}
	// Gate on the AgentClass's showAssistantStream, like every other stream
	// consumer: raw, free-form model narrative must not reach a surface unless
	// the class explicitly turns it on. The relay does NOT gate upstream — it
	// asks this method and drops the deltas on a nil sink — so an
	// unconditionally-returned sink would stream raw thinking to the TUI
	// whatever the flag says.
	if !channelkinds.ShowsAssistantStream(ctx, h.deps.K8sClient, sess) {
		return nil, nil
	}
	return &streamDeltaSink{sink: h.sink}, nil
}
