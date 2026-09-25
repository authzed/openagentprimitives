package browser

import (
	"context"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/clienthosted"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// HostConfig is the input to NewHost.
type HostConfig struct {
	// Deps is the generic channelkinds.Deps the host process builds. This kind
	// ignores Secret/AssetFetcher/AuthzReader/watchdog hooks. The view minters
	// feed their matching *_offer sub-channel senders; a nil SessionViewMinter
	// makes that sender fail loudly rather than drop the offer.
	Deps channelkinds.Deps
	// Sink is the EventSink the senders push render events into. Required.
	// pkg/web/webui/chat supplies the websocket-backed implementation
	// (its wsSink) that forwards these events to the browser page.
	Sink EventSink
	// User is the local operator's identity — the session's started_by.
	// ExternalID should be a stable per-machine id.
	User channelkinds.ExternalIdentity
	// Namespace and SessionName address the AgentSession this host's
	// Listener submits view_message requests to over NATS request-reply
	// (ap.session.<Namespace>.<SessionName>.in.view_message). They are
	// distinct from the Channel's own (namespace, name) — the ephemeral
	// browser Channel is named "<session>-chan", not the session name
	// itself — so they cannot be derived from Deps.Channel.
	Namespace   string
	SessionName string
	// Principal is the resolved identity for this chat session. Used by
	// the live_view_offer sub-channel sender to mint a signed webd deep-link
	// scoped to the session user. Optional: when zero-value, the link is
	// minted with an anonymous principal (no subject claim).
	Principal identity.Principal
	// Via is this surface's view URN (viewurn.Format(viewurn.TypeChat, "",
	// "")) — stamped on every SubmitUserMessage so the pipeline and the
	// agent's context know which surface originated the inbound. Optional:
	// zero-value means no via is attached.
	Via string
}

// Host binds the generic Deps to a concrete EventSink and exposes the
// host-side Listener/Sender objects the browser session's host process
// runs. It also satisfies outbound.SenderResolver: a browser session
// always routes to this one in-process host, so resolution ignores the
// AgentSession argument.
type Host struct {
	deps        channelkinds.Deps
	sink        EventSink
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
		return nil, fmt.Errorf("browser.NewHost: sink is required")
	}
	if cfg.Deps.Channel == nil {
		return nil, fmt.Errorf("browser.NewHost: deps.channel is required")
	}
	ch := cfg.Deps.Channel
	return &Host{
		deps:        cfg.Deps,
		sink:        cfg.Sink,
		user:        cfg.User,
		channelKey:  ChannelKey(ch.Namespace, ch.Name),
		principal:   cfg.Principal,
		ns:          cfg.Namespace,
		sessionName: cfg.SessionName,
		via:         cfg.Via,
	}, nil
}

// ChannelKey returns the stable per-session correlation key for a browser
// Channel. Exported so a host process can stamp the matching sha256-hashed
// label on the AgentSession it pre-creates (the pipeline correlates
// inbound -> session via channel.name + sha256(channelKey)).
func ChannelKey(ns, channelName string) string {
	return "browser:" + ns + "/" + channelName
}

// Listener returns the host-side Listener the browser page drives.
func (h *Host) Listener() *browserListener {
	return &browserListener{Listener: clienthosted.Listener{
		Deps:        h.deps,
		Ext:         h.user,
		ChannelKey:  h.channelKey,
		Namespace:   h.ns,
		SessionName: h.sessionName,
		Via:         h.via,
		Kind:        KindName,
	}}
}

// SenderFor implements outbound.SenderResolver — returns the main
// host-side Sender. The AgentSession argument is ignored: a browser
// session always routes to this host.
func (h *Host) SenderFor(context.Context, *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error) {
	return &browserSender{sink: h.sink}, nil
}

// SubChannelSenderFor implements outbound.SenderResolver — returns the
// host-side sub-channel sender for the named sub-channel, nil for
// unknown names.
//
// There is deliberately no case for channelkinds.SubChannelAgentUIOffer:
// this kind answers registry.IsBrowserSurface true, and a viewer already on
// a browser-hosted page has nothing left to open a link for — the surface
// this sub-channel would hand them a link to IS the page they're looking
// at.
func (h *Host) SubChannelSenderFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession, name string) (channelkinds.Sender, error) {
	switch name {
	case "message":
		return &browserSender{sink: h.sink}, nil
	case "tool_session":
		return &toolSessionSender{sink: h.sink}, nil
	case "permission_request":
		return &permissionRequestSender{sink: h.sink}, nil
	case "queued_messages":
		return &queuedMessagesSender{sink: h.sink}, nil
	case "interaction":
		return &interactionSender{sink: h.sink}, nil
	case string(channelevents.KindUserEcho):
		return &userEchoSender{sink: h.sink}, nil
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
	default:
		return nil, nil
	}
}

// StreamDeltaSinkFor implements outbound.SenderResolver — returns the
// host-side StreamDeltaSink, or nil when the session's AgentClass has not
// opted into showAssistantStream.
func (h *Host) StreamDeltaSinkFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.StreamDeltaSink, error) {
	// Gate on the AgentClass's showAssistantStream, like every other stream
	// consumer: the page must NOT render the agent's raw, free-form LLM
	// narrative unless the class opts in. The relay does not gate upstream — it
	// drops the deltas when this returns a nil sink — so an
	// unconditionally-returned sink would stream raw thinking whatever the flag
	// says.
	if !channelkinds.ShowsAssistantStream(ctx, h.deps.K8sClient, sess) {
		return nil, nil
	}
	return &streamDeltaSink{sink: h.sink}, nil
}
