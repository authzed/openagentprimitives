// Package clienthosted holds the plumbing shared by the client-hosted channel
// kinds — `local` (the `oap` TUI) and `browser` (the built-in web chat).
//
// A client-hosted kind is one whose Listener/Sender/StreamDeltaSink are NOT
// hosted by channelsd (RelayedByChannelsd is false). They run inside the
// process that owns the surface: `oap` for the terminal UI, webd for the browser
// page. channelsd still registers them so it recognises and skips them rather
// than erroring on an unknown kind.
//
// What lives here is what is genuinely ONE implementation across both: the
// inbound Listener and the single-user AudienceResolver. What deliberately does
// NOT is each kind's Sender family and render-event vocabulary — those
// translate an envelope into their own host's language (websocket JSON frames
// for the browser, tea.Msg values for bubbletea), so collapsing them would mean
// rewriting both consumers rather than sharing a behaviour. See the `local` and
// `browser` package docs for the load-bearing differences.
package clienthosted

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// Listener is the host-side channelkinds.Listener for a client-hosted kind.
// Unlike the channelsd kinds it is not poll/socket-driven — Start/Stop are
// no-ops and the surface pushes input in by calling the Submit* methods
// directly. It owns the channelKey, the local user identity, and the
// (Namespace, SessionName) it addresses channelsd with.
//
// Kind is the owning kind's name; it appears only in error messages, so a
// failure names the surface the user is actually looking at.
type Listener struct {
	Deps channelkinds.Deps
	// Ext is this listener's OWN configured identity. The Submit* methods do
	// NOT read it — each takes the identity to stamp as an explicit per-call
	// argument, because a client-hosted surface can be shared by more than one
	// subject across its lifetime (the browser chat re-attaches one entry's
	// listener to whichever subject holds agentsession#interact). Ext is here
	// for a caller with no independent per-call identity: the single-user TUI's
	// overrides read it and pass it through.
	Ext         channelkinds.ExternalIdentity
	ChannelKey  string // stable per-session correlation key
	Namespace   string // session namespace
	SessionName string // session name
	Via         string // this surface's view URN (e.g. urn:ap:view:tui)
	Kind        string // owning kind name, for error messages
}

// Start is a no-op: the transport is the surface itself, which drives the
// listener directly. Implemented for channelkinds.Listener compliance.
func (l *Listener) Start(context.Context) error { return nil }

// Stop is a no-op. The host process's lifecycle owns teardown.
func (l *Listener) Stop(context.Context) error { return nil }

// SubmitUserMessage submits the typed message to channelsd over NATS
// request-reply and returns the InboundDecision so the caller can render a deny
// message.
//
// ext is the identity to stamp as the message's author, supplied per call
// rather than read from l.Ext — see the Ext field's doc.
//
// requestID is the client's idempotency key: channelsd returns the first
// delivery's cached decision for a repeat id, so a reply lost to a transient
// timeout cannot double-post. A surface with no retry affordance may pass "",
// which skips the dedup cache entirely.
//
// It does NOT run an in-process pipeline. Appending the inbound turn requires
// the session's Ed25519 audit-signing seed, and neither a laptop nor a
// browser-facing process may hold a credential that can forge the audit chain.
// channelsd subscribes ap.session.*.*.in.<kind> cluster-wide and is the sole
// inbound writer.
func (l *Listener) SubmitUserMessage(_ context.Context, ext channelkinds.ExternalIdentity, text, requestID string) (channelkinds.InboundDecision, error) {
	dec, err := channelkinds.RequestViewMessage(l.Deps, l.Namespace, l.SessionName, text, l.Via, ext, false, requestID)
	if err != nil {
		return dec, fmt.Errorf("%s listener: %w", l.Kind, err)
	}
	return dec, nil
}

// SubmitInteractionDecision publishes a KindInteractionDecision envelope on the
// session's IN subject. channelsd's category-generic decision pipe validates
// the decider's standing per the category's DeciderPolicy, invokes the bound
// handler, and re-emits a KindInteractionApplied envelope the surface sees via
// the outbound relay.
//
// ext is stamped as Decider, supplied per call rather than read from l.Ext. A
// wrong Decider here is not cosmetic: DeciderPolicy validates standing against
// it, so it decides whose authority the decision is evaluated under.
func (l *Listener) SubmitInteractionDecision(_ context.Context, ext channelkinds.ExternalIdentity, ns, name, category, requestRef, actionID string) error {
	if l.Deps.NATSPublish == nil {
		return fmt.Errorf("%s listener: no NATS publish wired", l.Kind)
	}
	pl := channelevents.InteractionDecisionPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: name},
		Category:        category,
		RequestRef:      requestRef,
		ActionID:        actionID,
		Decider: channelevents.ExternalIdentity{
			Kind:       ext.Kind,
			ExternalID: ext.ExternalID,
			Email:      ext.Email,
			TeamScope:  ext.TeamScope,
		},
	}
	if err := channelevents.PublishIn(l.Deps.NATSPublish, ns, name,
		channelevents.KindInteractionDecision, pl); err != nil {
		return fmt.Errorf("%s listener: publish interaction_decision: %w", l.Kind, err)
	}
	return nil
}

// SubmitInterrupt publishes a KindInterruptRequest envelope on the session's IN
// subject, requesting a mid-turn interrupt of the agent's in-flight work. This
// in-process listener publishes directly to the runner's IN subject — no
// channelsd hop — and the runner's own NATS subscription decides whether the
// current turn is interruptible, then re-emits a KindInterruptApplied envelope
// the surface sees via the outbound relay.
//
// ext is stamped as Requester — supplied by the caller, not read from l.Ext;
// see SubmitUserMessage's doc for why.
func (l *Listener) SubmitInterrupt(_ context.Context, ext channelkinds.ExternalIdentity, ns, name, requestID string) error {
	if l.Deps.NATSPublish == nil {
		return fmt.Errorf("%s listener: no NATS publish wired", l.Kind)
	}
	pl := channelevents.InterruptRequestPayload{
		RequestID:  requestID,
		SessionRef: ns + "/" + name,
		Requester: channelevents.ExternalIdentity{
			Kind:       ext.Kind,
			ExternalID: ext.ExternalID,
			Email:      ext.Email,
			TeamScope:  ext.TeamScope,
		},
	}
	if err := channelevents.PublishIn(l.Deps.NATSPublish, ns, name,
		channelevents.KindInterruptRequest, pl); err != nil {
		return fmt.Errorf("%s listener: publish interrupt_request: %w", l.Kind, err)
	}
	return nil
}

// SubmitResurface asks channelsd to re-deliver whatever prompt the session is
// currently parked on. Both surfaces have the same gap: the browser's sink is
// live-only by design, and the TUI's outbound relay only subscribes after the
// AgentSession has been created and waited on, so a prompt published in that
// window went out to nobody and the deduped publisher never re-sends it. See
// channelkinds.PublishResurfaceRequest.
func (l *Listener) SubmitResurface(_ context.Context, ns, name string) error {
	if err := channelkinds.PublishResurfaceRequest(l.Deps, ns, name, l.Via); err != nil {
		return fmt.Errorf("%s listener: %w", l.Kind, err)
	}
	return nil
}

// SingleUserAudience is the CapabilitySingleUser AudienceResolver both
// client-hosted kinds use: the audience is always the session initiator, the one
// user driving the surface. Kind names the owning kind in the error message.
type SingleUserAudience struct {
	Kind string
}

// AudienceCapability returns SingleUser; no I/O.
func (SingleUserAudience) AudienceCapability() channelkinds.Capability {
	return channelkinds.CapabilitySingleUser
}

// ResolveAudience returns the session initiator as the sole audience subject.
func (a SingleUserAudience) ResolveAudience(_ context.Context, sess channelkinds.SessionInfo) ([]string, error) {
	if sess.SessionInitiator.IsZero() {
		return nil, fmt.Errorf("%s audience: SessionInitiator missing", a.Kind)
	}
	// identity boundary: AudienceResolver.ResolveAudience returns []string; a typed []identity.CanonicalUserID retype is deferred (typed-identity follow-up).
	return []string{sess.SessionInitiator.String()}, nil
}

// Compile-time checks.
var (
	_ channelkinds.Listener         = (*Listener)(nil)
	_ channelkinds.AudienceResolver = SingleUserAudience{}
)
