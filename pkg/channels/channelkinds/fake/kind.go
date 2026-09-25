// Package fake is an in-process channel kind for integration tests.
// Inject inbound events via the Driver; observe outbound events via Driver.Sent().
//
// NEVER use in production. The Driver is a process-global registry keyed by
// (namespace, channelName); tests should use distinct names to avoid bleed.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

func init() { registry.Register(&Kind{}) }

type Kind struct{}

func (Kind) Name() string                { return "fake" }
func (Kind) DefaultSessionScope() string { return "user" }

// Capabilities is the minimal set by default — widening it would change the
// meta-tool list of every bundle in the suite. A run that opted in via
// EnableDeliverySurfaces additionally advertises the asset capability
// respond_to_user's `attached` field is gated on. See delivery_surfaces.go.
func (Kind) Capabilities() []string {
	caps := []string{"text", "markdown"}
	if deliverySurfaces.Load() {
		caps = append(caps, deliveryCapabilities...)
	}
	return caps
}

func (Kind) NewListener(deps channelkinds.Deps) channelkinds.Listener {
	return &fakeListener{deps: deps, drv: driverFor(deps.Channel)}
}

func (Kind) NewSender(deps channelkinds.Deps) channelkinds.Sender {
	return &fakeSender{deps: deps, drv: driverFor(deps.Channel)}
}

// SubChannelSender returns the recording Sender for the named sub-channel. Each
// appends its envelopes to a Driver queue the e2e harness drains:
//
//	message             the standard message sender
//	permission_request  sessionJoinPrompts (ExpectSessionJoinPrompt)
//	credential_request  Driver.CredentialRequests()
//	credential_linked   Driver.CredentialLinkeds()
//	queued_messages     Driver.EnqueueAcks() + Driver.InterruptApplieds()
//	user_echo           Driver.UserEchoes()
//	interaction         Driver.InteractionPrompts() + Driver.InteractionApplieds()
//
// "interaction" is the generic Interaction model's single sub-channel, carrying
// both the request and applied legs; tool approval, info leakage and portal
// access all render through it rather than through sub-channels of their own.
//
// The view surfaces (browser, local, bento) do not implement user_echo — they
// ARE the origin surface a view-originated message would be echoed back into.
//
// Unknown sub-channel names return nil so the relay degrades gracefully.
func (Kind) SubChannelSender(name string, deps channelkinds.Deps) channelkinds.Sender {
	switch name {
	case "message":
		return &fakeSender{deps: deps, drv: driverFor(deps.Channel)}
	case "permission_request":
		return &fakePermReqSender{deps: deps, drv: driverFor(deps.Channel)}
	case string(channelevents.KindCredentialRequest):
		return newCredentialRequestSender(driverFor(deps.Channel))
	case string(channelevents.KindCredentialLinked):
		return newCredentialLinkedSender(driverFor(deps.Channel))
	case "queued_messages":
		return &fakeQueuedMessagesSender{deps: deps, drv: driverFor(deps.Channel)}
	case string(channelevents.KindUserEcho):
		return &fakeUserEchoSender{deps: deps, drv: driverFor(deps.Channel)}
	case "interaction":
		return newInteractionSender(driverFor(deps.Channel))
	case "live_view_offer":
		// Only while the delivery surfaces are opted in — the same flag
		// SupportsLiveViewOffer reads, so the declaration and the sender can
		// never disagree.
		if !deliverySurfaces.Load() {
			return nil
		}
		return &fakeLiveViewOfferSender{deps: deps}
	}
	return nil
}

// NewStreamDeltaSink: the fake kind opts out of stream-delta
// rendering. Tests that exercise stream paths use the slack kind via
// fixtures; the fake driver records discrete envelopes only.
func (Kind) NewStreamDeltaSink(channelkinds.Deps) channelkinds.StreamDeltaSink { return nil }

// SupportsMonitoring is true: NewMonitoringSender below returns a real sender.
func (Kind) SupportsMonitoring() bool { return true }

// SupportsLiveViewOffer answers the delivery-surfaces opt-in, and must keep
// agreeing with SubChannelSender("live_view_offer", …) in BOTH states — the two
// read the same flag for that reason, and TestFake_LiveViewDeclarationMatchesItsSender
// pins it. A kind claiming a surface with no sender behind it is the same
// defect artifact_offer_view's outcomes exist to expose.
//
// False by default, which is honest: without the opt-in there is no sender, so
// an offer published here is dropped. That default is also what lets a scenario
// exercise the "this channel has no live-view surface" path a real transport
// without one would take.
func (Kind) SupportsLiveViewOffer() bool { return deliverySurfaces.Load() }

// NewMonitoringSender returns the fake MonitoringSender — it records
// every event into the Channel's Driver for test assertions.
func (Kind) NewMonitoringSender(deps channelkinds.Deps) channelkinds.MonitoringSender {
	return &fakeMonitoringSender{deps: deps, drv: driverFor(deps.Channel)}
}

// fakeMonitoringSender is the fake kind's MonitoringSender. It appends
// each event to the Driver's monitoringEvents slice.
type fakeMonitoringSender struct {
	deps channelkinds.Deps
	drv  *Driver
}

func (s *fakeMonitoringSender) SendMonitoring(_ context.Context, ev channelevents.MonitoringEvent) error {
	s.drv.mu.Lock()
	s.drv.monitoringEvents = append(s.drv.monitoringEvents, ev)
	s.drv.mu.Unlock()
	return nil
}

// SupportedRoles is every role: the in-process test kind records whatever it
// is given in either direction, and the e2e harness binds it as input, output
// and both. Stated as the full set rather than nil — nil means "serves no
// role", which is not what this kind means.
func (Kind) SupportedRoles() []string { return spiceboxv1alpha1.AllChannelRoles() }

// ValidateSpec rejects channels that mix fake with slack's spec
// block. spec.fake itself stays optional (existing fixtures default
// to {} and a missing block is treated as defaults).
func (Kind) ValidateSpec(ch *spiceboxv1alpha1.Channel) error {
	if ch.Spec.Slack != nil {
		return errors.New(`spec.slack must be empty when kind="fake"`)
	}
	return nil
}

// RequiredSecretKeys: fake channels need no Secret data keys.
func (Kind) RequiredSecretKeys(*spiceboxv1alpha1.Channel) []string { return nil }

// PublicSecretKeys is nil: this kind declares no Secret shape, so it can
// vouch for no key in one. Anything an operator put there stays secret.
func (Kind) PublicSecretKeys(*spiceboxv1alpha1.Channel) []string { return nil }

// FeatureSupport reports no transport permissions: this kind has no
// permission model, so every feature it can express is free.
func (Kind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return nil
}

// RenderMention: fake has no native @-tag syntax; return the bare ID.
func (Kind) RenderMention(externalID string) string { return externalID }

// SupportedMentionLookups, LookupUser and MentionToolDescription live in
// mention_standin.go: the fake kind advertises no directory by default and
// serves a seeded one when a replay turns it on.

// TextFormattingInstructions implements channelkinds.TextFormatter. The fake
// transport records text verbatim for assertions, so it deliberately returns
// the generic instructions: e2e scenarios assert on what the agent wrote, and
// a bespoke dialect here would make fixtures diverge from every real surface.
func (Kind) TextFormattingInstructions() string {
	return channelkinds.DefaultTextFormattingInstructions
}

func (Kind) UserAttributable() bool { return true }

// DeliversToHuman is true. The fake transport stands in for a human-facing
// surface: its senders record what a person would have been shown, and the
// e2e scenarios answer its interaction cards as that person. Answering false
// would route every prompt in a fake-bound scenario past the very sender the
// scenario asserts on.
func (Kind) DeliversToHuman() bool { return true }

// AllowsSyntheticIdentity: false — matches production security posture. The
// e2e harness uses email-bearing identities; the fake kind must not mask a
// regression in the real (slack/browser) fail-closed behavior by quietly
// allowing synthetic subjects.
func (Kind) AllowsSyntheticIdentity() bool { return false }

// RelayedByChannelsd: the fake kind runs in-process inside channelsd
// (and the e2e harness's channelsd-equivalent). True.
func (Kind) RelayedByChannelsd() bool { return true }

// AttributesOrgMembership: opt-in per Channel via spec.fake.orgScoped, so
// bundles can exercise the session-start gate while ordinary test channels
// keep their pre-gate behavior. On an org-scoped fake channel the injected
// identity's stamp is authoritative — an unstamped injection reads as guest
// at the pipeline (fail-closed), and a bundle simulates a member by
// injecting OrgMembershipMember explicitly.
func (Kind) AttributesOrgMembership(ch *spiceboxv1alpha1.Channel) bool {
	return ch != nil && ch.Spec.Fake != nil && ch.Spec.Fake.OrgScoped
}

// SpawnsSessionOnInbound: the fake kind models a durable channel for
// tests and e2e — an inbound on a fresh key spawns a new session, just
// like slack. True.
func (Kind) SpawnsSessionOnInbound() bool { return true }

// WebAuthenticator returns a scriptable test authenticator. Tests call
// SetCanonicalForState before driving the OIDC flow; Complete looks up
// the scripted canonical subject for the given state.
func (Kind) WebAuthenticator(deps channelkinds.WebAuthDeps) channelkinds.WebAuthenticator {
	return &fakeAuth{externalBaseURL: deps.ExternalBaseURL}
}

// WebhookReceiver returns nil: this kind has no inbound HTTP surface.
func (Kind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver { return nil }

// Wizard returns the `oap channel create --kind fake` flow. Implemented in
// wizard.go.
func (Kind) Wizard() channelkinds.Wizard { return &fakeWizard{} }

// Driver lets tests inject inbound and observe outbound for a given Channel CR.
type Driver struct {
	mu       sync.Mutex
	inboundQ chan channelkinds.InboundEvent
	sent     []channelevents.OutboundUserMessagePayload
	plans    []channelevents.PlanUpdatePayload
	denials  []string
	// sessionJoinPrompts records every KindPermissionRequest envelope
	// (multiplayer session-join requests) the sub-channel sender received. It
	// has its own queue rather than sharing interactionPrompts because the
	// envelope kind, payload shape, and approver resolution are independent of
	// the interaction flow. Tests drain via SessionJoinPrompts().
	sessionJoinPrompts []recordedSessionJoinPrompt
	// monitoringEvents records every MonitoringEvent the fake monitoring
	// sender received. Tests drain via MonitoringEvents().
	monitoringEvents []channelevents.MonitoringEvent
	// credentialRequests records every KindCredentialRequest envelope the
	// credential_request sub-channel sender received, so the self-service E2E
	// can assert channelsd's watcher published the right fields. Tests drain
	// via CredentialRequests().
	credentialRequests []recordedCredentialRequest
	// credentialLinkeds records every KindCredentialLinked envelope the
	// credential_linked sub-channel sender received, so the E2E can assert
	// channelsd's OOB confirmation watcher published one after a credential was
	// added or replaced on a UserIdentity. Tests drain via CredentialLinkeds().
	credentialLinkeds []recordedCredentialLinked
	// enqueueAcks records every KindEnqueueAck envelope the
	// queued_messages sub-channel sender received — channelsd's
	// proactive "your message is queued" ack for a mid-turn inbound
	// that hit a Running session. Tests drain via Driver.EnqueueAcks().
	enqueueAcks []recordedEnqueueAck
	// interruptApplieds records every KindInterruptApplied envelope the
	// queued_messages sub-channel sender received — the runner's
	// outcome for a channel-initiated interrupt request. Tests drain
	// via Driver.InterruptApplieds().
	interruptApplieds []recordedInterruptApplied
	// audience is the per-Driver scripted leakage-gate audience.
	// SetAudience writes; ResolveAudience reads. Tests use this to drive
	// the write-side gate to specific recipient sets without depending
	// on real channel-membership state.
	audience []string
	// notifications records every KindNotification envelope the main
	// sender (fakeSender) received — runner-side Notify(...) calls that
	// ride the notification envelope rather than a user_message reply
	// (mid-turn status pings, the toolAuthDisabled warning, the
	// post-session cost report). Tests drain via Driver.Notifications().
	notifications []channelevents.NotificationPayload
	// userEchoes records every KindUserEcho envelope the user_echo
	// sub-channel sender received — the outbound mirror of a
	// view-originated message back into the origin channel (channelsd's
	// view_message pipeline publishes this after a routed Deliver). Tests
	// drain via Driver.UserEchoes().
	userEchoes []recordedUserEcho
	// interactionPrompts records every KindInteractionRequest envelope the
	// interaction sub-channel sender received — the generic Interaction model's
	// request leg, through which every prompt category funnels. Tests drain via
	// Driver.InteractionPrompts(). Recorded type and accessor live in
	// interaction.go, alongside the sender.
	interactionPrompts []recordedInteractionPrompt
	// interactionApplieds records every KindInteractionApplied envelope the
	// interaction sub-channel sender received — published by
	// HandleInteractionDecision once a category's bound decision handler
	// resolves the request. Tests drain via Driver.InteractionApplieds().
	interactionApplieds []recordedInteractionApplied
	// liveViewOffers records every KindLiveViewOffer envelope the
	// live_view_offer sub-channel sender received. That sender exists only
	// while EnableDeliverySurfaces is in effect, so this stays empty for every
	// bundle that did not opt in — see delivery_surfaces.go.
	liveViewOffers []recordedLiveViewOffer
}

// recordedSessionJoinPrompt pairs a KindPermissionRequest payload with
// the session ref it concerns. The e2e harness's ExpectSessionJoinPrompt
// helper drains these to drive the multiplayer-join approval flow:
// User A starts a session, User B sends an inbound, the pipeline
// emits a KindPermissionRequest → permission_request sub-channel
// sender → here.
type recordedSessionJoinPrompt struct {
	Payload    channelevents.PermissionRequestPayload
	SessionRef channelkinds.SessionInfo
}

// recordedUserEcho pairs a KindUserEcho payload with the session ref it
// was bound to. The user_echo sub-channel sender records these so tests
// (and the e2e harness) can assert the origin channel received the
// view-originated mirror.
type recordedUserEcho struct {
	Payload    channelevents.UserEchoPayload
	SessionRef channelkinds.SessionInfo
}

// recordedCredentialRequest pairs a KindCredentialRequest payload with the
// session ref it concerns.
type recordedCredentialRequest struct {
	Payload    channelevents.CredentialRequestPayload
	SessionRef channelkinds.SessionInfo
}

// recordedCredentialLinked pairs a KindCredentialLinked payload with the
// session ref it was bound to.
type recordedCredentialLinked struct {
	Payload    channelevents.CredentialLinkedPayload
	SessionRef channelkinds.SessionInfo
}

// recordedEnqueueAck pairs a KindEnqueueAck payload with the session ref
// it concerns. The e2e harness's ExpectEnqueueAck helper drains
// Driver.EnqueueAcks() to observe channelsd's proactive "your message is
// queued" ack for a mid-turn inbound that hit a Running session.
type recordedEnqueueAck struct {
	Payload    channelevents.EnqueueAckPayload
	SessionRef channelkinds.SessionInfo
}

// recordedInterruptApplied pairs a KindInterruptApplied payload with the
// session ref it concerns. The e2e harness's Interrupt.WaitInterruptApplied
// helper drains Driver.InterruptApplieds() to observe the runner's outcome
// for a channel-initiated interrupt click.
type recordedInterruptApplied struct {
	Payload    channelevents.InterruptAppliedPayload
	SessionRef channelkinds.SessionInfo
}

var (
	drvMu   sync.Mutex
	drivers = map[string]*Driver{}
)

func driverFor(ch *spiceboxv1alpha1.Channel) *Driver {
	drvMu.Lock()
	defer drvMu.Unlock()
	key := ch.Namespace + "/" + ch.Name
	d, ok := drivers[key]
	if !ok {
		d = &Driver{inboundQ: make(chan channelkinds.InboundEvent, 16)}
		drivers[key] = d
	}
	return d
}

// DriverFor is a public accessor for tests.
func DriverFor(namespace, name string) *Driver {
	drvMu.Lock()
	defer drvMu.Unlock()
	return drivers[namespace+"/"+name]
}

// ResetAllDrivers clears the process-global driver registry. The
// e2e harness calls this from its per-test cleanup so that
// `go test -count=N` runs of the same scenario don't see stale
// outbound/inbound state from earlier iterations. Safe to call when
// no drivers exist.
//
// Production code MUST NOT call this — the registry is shared with
// any other tests in the same process and clearing it mid-run breaks
// in-flight listeners. The harness's cleanup runs after all per-test
// goroutines have wound down (manager cancel + done-wait).
func ResetAllDrivers() {
	drvMu.Lock()
	defer drvMu.Unlock()
	drivers = map[string]*Driver{}
}

func (d *Driver) Inject(ev channelkinds.InboundEvent) { d.inboundQ <- ev }

func (d *Driver) Sent() []channelevents.OutboundUserMessagePayload {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]channelevents.OutboundUserMessagePayload(nil), d.sent...)
}

// Plans returns a snapshot of every PlanUpdatePayload received by this
// Driver's senders.
func (d *Driver) Plans() []channelevents.PlanUpdatePayload {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]channelevents.PlanUpdatePayload(nil), d.plans...)
}

func (d *Driver) Denials() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.denials...)
}

// Notifications returns a snapshot of every KindNotification envelope
// recorded by this Driver's main sender (fakeSender). Used by e2e
// scenarios asserting on runner-side Notify(...) text — status pings, the
// toolAuthDisabled warning, and the post-session cost report all ride this
// envelope kind rather than the KindUserMessage Sent() observes.
func (d *Driver) Notifications() []channelevents.NotificationPayload {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]channelevents.NotificationPayload(nil), d.notifications...)
}

// SessionJoinPrompt is a public projection of one captured
// KindPermissionRequest (multiplayer session-join) envelope.
type SessionJoinPrompt struct {
	Payload    channelevents.PermissionRequestPayload
	SessionRef channelkinds.SessionInfo
}

// SessionJoinPrompts returns a snapshot of every KindPermissionRequest
// envelope recorded by this Driver's permission_request sub-channel
// sender. Append-only; subsequent calls return additional entries as
// they arrive. Separate from ApprovalPrompts (tool-approval flow)
// because session-join and tool-approval are independent pipelines
// that happen to share the permission_request sub-channel.
func (d *Driver) SessionJoinPrompts() []SessionJoinPrompt {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]SessionJoinPrompt, len(d.sessionJoinPrompts))
	for i, p := range d.sessionJoinPrompts {
		out[i] = SessionJoinPrompt{Payload: p.Payload, SessionRef: p.SessionRef}
	}
	return out
}

// UserEcho is a public projection of one captured KindUserEcho envelope —
// the outbound mirror of a view-originated message back into the origin
// channel.
type UserEcho struct {
	Payload    channelevents.UserEchoPayload
	SessionRef channelkinds.SessionInfo
}

// UserEchoes returns a snapshot of every KindUserEcho envelope recorded by
// this Driver's user_echo sub-channel sender. Append-only; subsequent
// calls return additional entries as they arrive.
func (d *Driver) UserEchoes() []UserEcho {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]UserEcho, len(d.userEchoes))
	for i, p := range d.userEchoes {
		out[i] = UserEcho{Payload: p.Payload, SessionRef: p.SessionRef}
	}
	return out
}

// CredentialRequestRecord is a public projection of one captured
// KindCredentialRequest envelope, for asserting on the credential-link prompt
// channelsd's watcher sends.
type CredentialRequestRecord struct {
	Payload    channelevents.CredentialRequestPayload
	SessionRef channelkinds.SessionInfo
}

// CredentialRequests returns a snapshot of every KindCredentialRequest
// envelope recorded by this Driver's credential_request sub-channel
// sender. Append-only; subsequent calls return additional entries as
// they arrive.
func (d *Driver) CredentialRequests() []CredentialRequestRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]CredentialRequestRecord, len(d.credentialRequests))
	for i, r := range d.credentialRequests {
		out[i] = CredentialRequestRecord{Payload: r.Payload, SessionRef: r.SessionRef}
	}
	return out
}

// CredentialLinkedRecord is a public projection of one captured
// KindCredentialLinked envelope, for asserting on the OOB confirmation
// channelsd's watcher publishes when UserIdentity.spec.credentials changes.
type CredentialLinkedRecord struct {
	Payload    channelevents.CredentialLinkedPayload
	SessionRef channelkinds.SessionInfo
}

// CredentialLinkeds returns a snapshot of every KindCredentialLinked
// envelope recorded by this Driver's credential_linked sub-channel
// sender. Append-only; subsequent calls return additional entries as
// they arrive.
func (d *Driver) CredentialLinkeds() []CredentialLinkedRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]CredentialLinkedRecord, len(d.credentialLinkeds))
	for i, r := range d.credentialLinkeds {
		out[i] = CredentialLinkedRecord{Payload: r.Payload, SessionRef: r.SessionRef}
	}
	return out
}

// MonitoringEvents returns a snapshot of every MonitoringEvent recorded
// by this Driver's monitoring sender. Append-only.
func (d *Driver) MonitoringEvents() []channelevents.MonitoringEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]channelevents.MonitoringEvent(nil), d.monitoringEvents...)
}

type fakeListener struct {
	deps channelkinds.Deps
	drv  *Driver
	stop chan struct{}
}

func (l *fakeListener) Start(ctx context.Context) error {
	l.stop = make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-l.stop:
				return
			case ev := <-l.drv.inboundQ:
				ev.Channel = l.deps.Channel
				ev.Reply = channelkinds.InboundReplyHooks{
					Ephemeral: func(_ context.Context, msg string) error {
						l.drv.mu.Lock()
						l.drv.denials = append(l.drv.denials, msg)
						l.drv.mu.Unlock()
						return nil
					},
				}
				_, _ = l.deps.Inbound.Deliver(ctx, ev)
			}
		}
	}()
	return nil
}

func (l *fakeListener) Stop(_ context.Context) error {
	if l.stop != nil {
		close(l.stop)
	}
	return nil
}

type fakeSender struct {
	deps channelkinds.Deps
	drv  *Driver
}

func (s *fakeSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	switch env.Kind {
	case channelevents.KindUserMessage:
		var pl channelevents.OutboundUserMessagePayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return channelkinds.SubChannelSendResult{}, err
		}
		s.drv.mu.Lock()
		s.drv.sent = append(s.drv.sent, pl)
		s.drv.mu.Unlock()

		if s.deps.Channel.Spec.Fake != nil && s.deps.Channel.Spec.Fake.Echo {
			s.drv.Inject(channelkinds.InboundEvent{
				ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "echo", Email: "echo@example.com"},
				ChannelKey:  "fake-echo",
				MessageText: pl.Text,
			})
		}
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindPlanUpdate:
		var pl channelevents.PlanUpdatePayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return channelkinds.SubChannelSendResult{}, err
		}
		s.drv.mu.Lock()
		s.drv.plans = append(s.drv.plans, pl)
		s.drv.mu.Unlock()
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindTurnProgress:
		return channelkinds.SubChannelSendResult{}, nil // render-only; fake channel ignores

	case channelevents.KindToolProgress:
		return channelkinds.SubChannelSendResult{}, nil // render-only; fake channel ignores

	case channelevents.KindNotification:
		var pl channelevents.NotificationPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return channelkinds.SubChannelSendResult{}, err
		}
		s.drv.mu.Lock()
		s.drv.notifications = append(s.drv.notifications, pl)
		s.drv.mu.Unlock()
		return channelkinds.SubChannelSendResult{}, nil

	default:
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake sender: unsupported kind %q", env.Kind)
	}
}

// fakePermReqSender is the "permission_request" sub-channel sender.
// Decodes incoming KindPermissionRequest (multiplayer session-join)
// envelopes and appends to the Driver's sessionJoinPrompts queue so the
// e2e harness can match against them via ExpectSessionJoinPrompt.
type fakePermReqSender struct {
	deps channelkinds.Deps
	drv  *Driver
}

func (s *fakePermReqSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	switch env.Kind {
	case channelevents.KindPermissionRequest:
		// Multiplayer session-join request: a non-started_by user tried
		// to interact with the session and failed CheckInteract; the
		// pipeline emits this so the started_by gets a DM and decides
		// whether to admit. The e2e harness's ExpectSessionJoinPrompt
		// matches against the queue this populates.
		var pl channelevents.PermissionRequestPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake permission_request: decode PermissionRequestPayload: %w", err)
		}
		s.drv.mu.Lock()
		s.drv.sessionJoinPrompts = append(s.drv.sessionJoinPrompts, recordedSessionJoinPrompt{
			Payload:    pl,
			SessionRef: sess,
		})
		s.drv.mu.Unlock()
		return channelkinds.SubChannelSendResult{}, nil
	case channelevents.KindPermissionDecisionApplied:
		// Applied envelopes update the user-facing UI in the slack
		// kind (mark the in-thread prompt resolved). The fake driver has no
		// UI to update — drop silently so the e2e relay doesn't error.
		return channelkinds.SubChannelSendResult{}, nil
	default:
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake permission_request sender: unsupported envelope kind %q", env.Kind)
	}
}

// fakeUserEchoSender is the "user_echo" sub-channel sender. Decodes
// incoming KindUserEcho envelopes — the outbound mirror of a
// view-originated message that channelsd's view_message pipeline
// publishes after a routed Deliver — and appends to the Driver's
// userEchoes queue so tests (and the e2e harness) can assert the origin
// channel received the mirror. Modeled on fakePermReqSender.
type fakeUserEchoSender struct {
	deps channelkinds.Deps
	drv  *Driver
}

func (s *fakeUserEchoSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	switch env.Kind {
	case channelevents.KindUserEcho:
		var pl channelevents.UserEchoPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake user_echo: decode UserEchoPayload: %w", err)
		}
		s.drv.mu.Lock()
		s.drv.userEchoes = append(s.drv.userEchoes, recordedUserEcho{
			Payload:    pl,
			SessionRef: sess,
		})
		s.drv.mu.Unlock()
		return channelkinds.SubChannelSendResult{}, nil
	default:
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake user_echo sender: unsupported envelope kind %q", env.Kind)
	}
}

// fakeQueuedMessagesSender is the "queued_messages" sub-channel sender for the
// mid-turn queue+confirm flow. It decodes KindEnqueueAck and
// KindInterruptApplied envelopes and appends them to the Driver's queues so the
// e2e harness's ExpectEnqueueAck / Interrupt.WaitInterruptApplied can observe
// them. There is no ephemeral UI to render or edit — recording into the Driver
// IS this kind's rendering.
//
// Slack does not use this path: its enqueue ack rides the generic
// interaction_request (category queued_messages) and its applied edit is driven
// by internal/cmd/channelsd/interrupt_applied_bridge.go. browser/local/fake use the
// KindEnqueueAck/KindInterruptApplied path this sender decodes.
type fakeQueuedMessagesSender struct {
	deps channelkinds.Deps
	drv  *Driver
}

func (s *fakeQueuedMessagesSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	switch env.Kind {
	case channelevents.KindEnqueueAck:
		var pl channelevents.EnqueueAckPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake queued_messages: decode EnqueueAckPayload: %w", err)
		}
		s.drv.mu.Lock()
		s.drv.enqueueAcks = append(s.drv.enqueueAcks, recordedEnqueueAck{
			Payload:    pl,
			SessionRef: sess,
		})
		s.drv.mu.Unlock()
		return channelkinds.SubChannelSendResult{}, nil
	case channelevents.KindInterruptApplied:
		var pl channelevents.InterruptAppliedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake queued_messages: decode InterruptAppliedPayload: %w", err)
		}
		s.drv.mu.Lock()
		s.drv.interruptApplieds = append(s.drv.interruptApplieds, recordedInterruptApplied{
			Payload:    pl,
			SessionRef: sess,
		})
		s.drv.mu.Unlock()
		return channelkinds.SubChannelSendResult{}, nil
	default:
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake queued_messages sender: unsupported envelope kind %q", env.Kind)
	}
}

// AudienceCapability declares the fake kind's leakage-gate capability.
// CapabilityFull so tests can exercise the multi-recipient leakage path
// (audience > 1) end-to-end. The actual audience returned by
// ResolveAudience is scripted per-Driver via SetAudience; tests opt out
// of leakage involvement by leaving the audience unset (an empty
// audience trivially satisfies any permitted set).
func (Kind) AudienceCapability() channelkinds.Capability { return channelkinds.CapabilityFull }

// ResolveAudience returns the per-Driver scripted audience for the
// Channel named by info.Channel. The fake kind has no real channel
// membership concept; tests set the audience via Driver.SetAudience to
// drive the leakage gate to specific audience subjects.
//
// A nil info.Channel or an unknown channel returns an empty audience (no leak;
// the gate publishes). SessionInfo does not carry the channel name, so the
// lookup goes by the registered driver whose channel matches the session's
// namespace rather than by the "<ns>/<name>" registry key driverFor uses.
func (Kind) ResolveAudience(_ context.Context, info channelkinds.SessionInfo) ([]string, error) {
	if info.Channel == nil {
		return nil, nil
	}
	drvMu.Lock()
	defer drvMu.Unlock()
	// SessionInfo doesn't carry the channel object; iterate the registry
	// scoped to this namespace + match against the binding's channel ref.
	// One channel per harness namespace is the common case, so this is
	// O(1) in practice.
	for key, d := range drivers {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) != 2 {
			continue
		}
		if parts[0] != info.Namespace {
			continue
		}
		if info.Channel.Name != "" && parts[1] != info.Channel.Name {
			continue
		}
		out := make([]string, len(d.audience))
		copy(out, d.audience)
		return out, nil
	}
	return nil, nil
}

// SetAudience scripts the audience subjects ResolveAudience returns for
// this Driver. Subjects are canonical user ids (e.g.
// "user:base64-email"); the leakage gate compares them against
// SpiceDBLookupSubjects results. Setting an empty slice means "no
// recipients" — audience trivially permitted, no leak detected.
func (d *Driver) SetAudience(subjects []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.audience = append(d.audience[:0:0], subjects...)
}

// EnqueueAck is a public projection of one captured KindEnqueueAck
// envelope. Used by the e2e harness's ExpectEnqueueAck helper.
type EnqueueAck struct {
	Payload    channelevents.EnqueueAckPayload
	SessionRef channelkinds.SessionInfo
}

// EnqueueAcks returns a snapshot of every KindEnqueueAck envelope recorded
// by this Driver's queued_messages sub-channel sender. Append-only;
// subsequent calls return additional entries as they arrive.
func (d *Driver) EnqueueAcks() []EnqueueAck {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]EnqueueAck, len(d.enqueueAcks))
	for i, p := range d.enqueueAcks {
		out[i] = EnqueueAck{Payload: p.Payload, SessionRef: p.SessionRef}
	}
	return out
}

// InterruptApplied is a public projection of one captured
// KindInterruptApplied envelope. Used by the e2e harness's
// Interrupt.WaitInterruptApplied helper.
type InterruptApplied struct {
	Payload    channelevents.InterruptAppliedPayload
	SessionRef channelkinds.SessionInfo
}

// InterruptApplieds returns a snapshot of every KindInterruptApplied
// envelope recorded by this Driver's queued_messages sub-channel sender.
// Append-only; subsequent calls return additional entries as they arrive.
func (d *Driver) InterruptApplieds() []InterruptApplied {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]InterruptApplied, len(d.interruptApplieds))
	for i, p := range d.interruptApplieds {
		out[i] = InterruptApplied{Payload: p.Payload, SessionRef: p.SessionRef}
	}
	return out
}
