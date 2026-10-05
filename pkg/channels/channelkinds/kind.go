// Package channelkinds defines the kind interface implemented by each
// channel-kind package (slack, browser, local, bento, fake). internal/cmd/channelsd
// consumes Kind.NewListener/NewSender; cmd/oap consumes Kind.Wizard().
package channelkinds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// MentionLookupKind discriminates the input shape for Kind.LookupUser.
// Channel kinds advertise which they accept via SupportedMentionLookups.
type MentionLookupKind string

const (
	MentionLookupEmail MentionLookupKind = "email"
	MentionLookupName  MentionLookupKind = "name"
	MentionLookupAny   MentionLookupKind = "any" // kind applies its own heuristic
)

// LookupDeps is what Kind.LookupUser needs at call time. Kept separate
// from the full channelkinds.Deps struct so the runner can build it from
// only the credentials Secret loaded at session setup — without depending
// on channelsd-only fields (inbound pipeline, watchdog hooks, etc.).
type LookupDeps struct {
	Secret *corev1.Secret
}

// Sentinel errors returned by Kind.LookupUser. The generic
// lookup_user_for_mention tool maps each to a distinct IsError message;
// callers can errors.Is() to distinguish.
var (
	ErrMentionNotFound    = errors.New("mention: no user matched the supplied identifier")
	ErrMentionAmbiguous   = errors.New("mention: name matched multiple users")
	ErrMentionUnsupported = errors.New("mention: lookup kind not supported on this channel")
)

// MentionCandidate is one candidate result from a name lookup: a user the
// tool offers to the agent for judgement rather than auto-picking. It
// arises two ways — a fuzzy near-miss (name close but not exact) or an
// exact-name ambiguity (several people share the name) — and in both the
// agent, not the code, decides who was meant.
type MentionCandidate struct {
	ExternalID  string
	DisplayName string
	// AccountType is the candidate's coarse workspace standing (e.g.
	// "member", "guest", "admin", "owner"), surfaced so the agent can
	// weigh who was meant — a wrong mention pings a real person, and a
	// full member is usually the intended target over a guest. Empty when
	// the producing kind has no standing concept. Descriptive only: no
	// authorization decision may branch on it.
	AccountType string
}

// MentionNearMissError is returned by Kind.LookupUser when a name lookup
// could not resolve to a single confident user but did find candidates
// worth offering. The lookup_user_for_mention tool renders the candidates
// — each with its mention token and standing — and the agent decides
// whether one is the intended person; the code deliberately does not
// auto-pick.
type MentionNearMissError struct {
	// Candidates are ranked best-first and capped by the producing kind.
	Candidates []MentionCandidate
	// Exact reports that every candidate matched the requested name
	// EXACTLY and the request was ambiguous across several people, as
	// opposed to the fuzzy case where candidates only approximate the
	// request. The tool words its guidance differently for each: an exact
	// ambiguity steers toward the full member (likely employee), while a
	// fuzzy near-miss cautions to use a token only if it is clearly the
	// same person.
	Exact bool
}

func (e *MentionNearMissError) Error() string {
	if e.Exact {
		return fmt.Sprintf("mention: name matched %d users; ambiguous", len(e.Candidates))
	}
	return fmt.Sprintf("mention: no exact name match; %d near-miss candidate(s)", len(e.Candidates))
}

// FeatureRequirement is what one channelfeatures.Feature costs on a given
// transport: the permissions it needs, where a human enables them, and what
// visibly breaks without them.
//
// Single source for the wizard's feature checkboxes, the generated app
// manifest, the runtime missing-scope check, and the ScopesValid condition
// message.
type FeatureRequirement struct {
	// Scopes are the transport permissions the feature needs. Empty means the
	// kind supports the feature with no extra permission.
	Scopes []string
	// Setup is one line telling a human where to grant those permissions.
	Setup string
	// Degrades says what stops working without them, so an operator reading
	// the ScopesValid=False message learns what is broken rather than just a
	// scope name. Required — a requirement nobody can act on is worse than none.
	Degrades string
}

// Kind is the kind interface implemented by each channel kind package.
type Kind interface {
	Name() string
	DefaultSessionScope() string
	Capabilities() []string

	NewListener(deps Deps) Listener
	NewSender(deps Deps) Sender

	// SubChannelSender returns a Sender for the named sub-channel
	// ("message", "permission_request", "agent_ui_offer" (see
	// SubChannelAgentUIOffer), ...). nil = "this kind doesn't implement
	// that sub-channel"; callers degrade gracefully.
	SubChannelSender(name string, deps Deps) Sender

	// NewStreamDeltaSink returns the kind's StreamDeltaSink for live
	// rendering of LLM stream events (text_delta, tool_use_start, …)
	// onto the channel surface. Return nil to opt out — the relay
	// drops KindAssistantStreamDelta envelopes for that kind silently.
	// Sinks own per-thread debounce / buffer state and are cached
	// per-Channel by channelsd.
	NewStreamDeltaSink(deps Deps) StreamDeltaSink

	// SupportsMonitoring declares whether this kind can deliver a
	// MonitoringEvent to a role=monitoring Channel — i.e. whether
	// NewMonitoringSender returns a real sender. A kind returning false here
	// MUST return nil from NewMonitoringSender and vice versa; the two are
	// one fact and any disagreement is a bug in the kind.
	//
	// It is declared separately because the credential-update watcher must
	// ask, BEFORE publishing, whether anyone could receive what it is about
	// to send, and it has no resolved Secret with which to construct a
	// sender. PublishMonitoring writes to a fixed NATS subject and returns
	// nil whether or not the relay finds a recipient, so without this the
	// watcher would record "card delivered" for a card that was dropped.
	SupportsMonitoring() bool

	// SupportsLiveViewOffer declares whether this kind can render a live-view
	// offer — the "open this artifact in your browser" surface the
	// live_view_offer sub-channel sender puts on the conversation.
	//
	// A kind answering false MUST return nil from
	// SubChannelSender("live_view_offer", …) and vice versa; the two are one
	// fact and any disagreement is a bug in the kind.
	//
	// It is DECLARED rather than derived from SubChannelSender for the same
	// reason SupportsMonitoring is. The caller that needs the answer is
	// artifact_offer_view, in the runner, which holds no Deps with which to
	// build a sender; and the client-hosted kinds (local, browser) return nil
	// from the bare SubChannelSender because their real senders are built by
	// their Host, in the user's own process, so a registry lookup would answer
	// "no surface" for two kinds that plainly have one.
	//
	// It is asked BEFORE the offer is published. The outbound relay drops an
	// envelope whose sub-channel sender is nil with nothing but a log line, so
	// without this the tool would report an offer "sent" on a channel that can
	// never show one — and the model repeats that to a user, who then looks for
	// a button nobody rendered.
	SupportsLiveViewOffer() bool

	// NewMonitoringSender returns the kind's MonitoringSender for
	// delivering cluster-level framework MonitoringEvents to a
	// role=monitoring Channel. Return nil to opt out — and say so in
	// SupportsMonitoring, which is the ONLY form of that answer callers
	// without a resolved Secret can see. The returned sender reads its
	// destination from deps.Channel.
	NewMonitoringSender(deps Deps) MonitoringSender

	// SupportedRoles declares which ChannelSpec.Role values this kind can
	// actually serve: github and bento are input-only, slack serves all four.
	// Values are the spiceboxv1alpha1.ChannelRole* constants, and a kind that
	// serves every role says so with spiceboxv1alpha1.AllChannelRoles() rather
	// than by returning nil — an empty answer means "this kind serves no role
	// at all", which is never what a kind means, and callers read it that way.
	//
	// It exists for the callers that hold a (kind, role) pair and NO full
	// Channel: `oap agent lint` judging a bundle's requires.channels long
	// before the wizard has collected the kind-specific spec block, and the
	// install planner behind it. Those callers cannot ask ValidateSpec — every
	// kind that needs a spec block refuses a block-less Channel for that
	// reason, so its verdict on such a Channel says nothing about the role —
	// and the alternative, inferring the legal roles from the shape of a
	// kind's error strings, is a switch on the kind wearing a disguise.
	//
	// ValidateSpec remains the ENFORCEMENT path; this is a declaration. The
	// two must not become separate sources of truth: a kind that refuses a
	// role in ValidateSpec MUST derive that refusal from this list (github and
	// bento both do), so there is exactly one list per kind and no way for the
	// declaration to drift from what reconcile actually enforces.
	//
	// IF YOU ARE ADDING A ROLE RULE TO A KIND THAT HAD NONE, READ THIS. The
	// four kinds returning AllChannelRoles() today have no role gate in
	// ValidateSpec, because they refuse no role and there is nothing to
	// derive. The moment your kind refuses one, you MUST do both of these:
	//
	//  1. Narrow this list, and write the gate as a lookup INTO it —
	//     `if !slices.Contains(k.SupportedRoles(), ch.Spec.Role)` — never as a
	//     restatement of the rule in ValidateSpec.
	//  2. Add a test naming the refused role and the expected outcome
	//     LITERALLY, not derived from this list. A test that walks
	//     SupportedRoles follows the list wherever it goes, so it cannot catch
	//     a list wrongly widened; only a case that hardcodes "role X is
	//     refused" can. github and bento each carry one.
	//
	// The two failure directions are not symmetric, which is why step 1 is an
	// obligation and not a style note. Narrowing this list while ValidateSpec
	// stays permissive makes the lint reject what the cluster would accept:
	// annoying, safe, and loud at authoring time. Restating the rule in
	// ValidateSpec while this list still says all four does the opposite — the
	// bundle lints CLEAN, install applies the Channel, and the controller then
	// sets Valid=False reason=SpecInvalid. That is the "lints clean, installs
	// clean, fails later" shape this method exists to eliminate, reintroduced.
	SupportedRoles() []string

	// ValidateSpec runs kind-specific spec validation against a Channel
	// CR (e.g. slack rejects ch.Spec.Fake != nil, fake rejects
	// ch.Spec.Slack != nil). Returns nil when valid. Called by the
	// channel-controller's Reconcile to populate the Valid=False
	// reason=SpecInvalid condition; returning an error here wins over
	// the controller's own generic checks.
	//
	// A kind whose SupportedRoles is narrower than the full set enforces that
	// here too, reading SupportedRoles rather than restating the rule.
	ValidateSpec(ch *spiceboxv1alpha1.Channel) error

	// RequiredSecretKeys lists the Secret data keys that MUST be
	// populated for the kind to function (e.g. slack: bot-token,
	// app-token). The channel controller checks each key exists and
	// fails Valid=False reason=SecretKeyMissing otherwise. Empty list
	// means the kind needs no Secret keys (fake).
	RequiredSecretKeys(ch *spiceboxv1alpha1.Channel) []string

	// PublicSecretKeys names the data keys of the kind's credentials Secret
	// that hold a PUBLIC identifier rather than secret material.
	//
	// It is the exact counterpart of credkind.Kind.PublicSecretKeys, and both
	// exist because "these bytes came out of a Secret" is provenance, not
	// sensitivity. A kind's credentials Secret routinely mixes the two: the
	// github kind's holds a private key and a webhook secret alongside an app
	// id and an installation id, and GitHub publishes both of the latter — the
	// installation id arrives in the body of every webhook delivery, so it
	// cannot be kept out of anything that records what was delivered.
	//
	// Both halves are needed rather than one, because one Secret is named by
	// both registries. A github Channel's credentials Secret is the same object
	// a type=githubApp credential points at, and either may be present without
	// the other — a Channel that mints its own tokens needs no AgentIdentity
	// credential at all. A consumer unions what the declarers tell it: a kind
	// that declares nothing is silent, which neither grants publicness nor
	// vetoes another declarer's answer about the same key.
	//
	// FAIL CLOSED. nil means EVERY key is secret, and nil is what a kind that
	// has not thought about it returns. This is an opt-out for named public
	// keys, never an opt-in for protection.
	PublicSecretKeys(ch *spiceboxv1alpha1.Channel) []string

	// FeatureSupport declares which channelfeatures.Feature values this kind
	// can satisfy, and what each costs. A feature absent from the map is one
	// this kind cannot provide at all — callers degrade rather than erroring,
	// since an agent may be bound to several kinds with different reach.
	//
	// Kinds with no permission model (fake, local, browser) return nil.
	FeatureSupport() map[channelfeatures.Feature]FeatureRequirement

	// RenderMention returns the kind's syntax for "@-tagging" the
	// given externalID. Slack: "<@USERID>". A kind with no native
	// mention syntax should return the bare ID. Used by generic
	// pipeline messages (deny strings etc.) so they don't switch on
	// kind themselves.
	RenderMention(externalID string) string

	// SupportedMentionLookups returns the set of MentionLookupKind values
	// this kind can resolve. Empty/nil → kind does not support user lookup;
	// the runner omits lookup_user_for_mention from sessions bound here.
	// Drives the JSON-schema enum on the tool's input.
	SupportedMentionLookups() []MentionLookupKind

	// LookupUser resolves a (kind, value) pair to an externalID and a
	// display name. Returns ErrMentionNotFound, ErrMentionAmbiguous, or
	// ErrMentionUnsupported per the sentinels in kind.go; a name lookup
	// MAY instead return *MentionNearMissError carrying close candidates
	// for the agent to judge. Non-sentinel errors surface to the agent as
	// IsError with the wrapped message (transport failures, malformed
	// Secret, etc.). Implementations MUST be safe for concurrent calls.
	LookupUser(ctx context.Context, deps LookupDeps, kind MentionLookupKind, value string) (externalID, displayName string, err error)

	// MentionToolDescription returns the kind-specific description text
	// used as Tool.Description() for lookup_user_for_mention. Empty
	// string falls back to a generic template (see meta.NewLookupUserForMention).
	MentionToolDescription() string

	// UserAttributable reports whether the kind's inbound messages carry a
	// per-user identity (a Slack user_id). Kinds returning false (bento, which
	// is purely scheduler-driven) MUST set Channel.spec.authzSubject: the
	// AgentClass validator rejects classes bound to a non-attributable input
	// Channel that has neither spec.authz.session.interactPermission on the
	// class nor an authzSubject on the Channel.
	UserAttributable() bool

	// DeliversToHuman reports whether a Channel of this kind is read by a
	// PERSON. False for the kinds whose far side is a machine: `bento`, driven
	// by a scheduler, and `agent`, whose counterparty is another AgentSession.
	//
	// The outbound relay asks this before routing a human-directed envelope —
	// the interaction family, whose answer is a person's decision on a
	// permission, a credential, or an identity. A conversational subagent is
	// bound to an `agent` Channel as its spec.inputChannel, so that Channel is
	// what the session's own outbound binding resolves to; routing a permission
	// prompt there would hand a human's decision to the parent AGENT. The relay
	// resolves those envelopes through v1alpha1.ResolveHumanDirectedBinding
	// instead, which climbs the lineage to the nearest ancestor whose binding
	// answers true here and drops loudly when there is none.
	//
	// Distinct from UserAttributable even though every kind's two answers
	// coincide today. That one asks whether INBOUND messages carry a per-user
	// identity and is load-bearing for the AgentClass binding validator; this
	// one asks whether OUTBOUND human-directed traffic has a reader. Fusing two
	// facts into one field is how a future author changes one and silently
	// breaks the other, so they stay separate.
	//
	// Required rather than an optional interface, deliberately: an optional
	// interface needs a default, and the safe default here is the opposite of
	// the majority answer — a new agent-to-agent kind that forgot to implement
	// it would be treated as human-readable and would misroute prompts. A
	// required method makes the compiler ask every kind, present and future.
	DeliversToHuman() bool

	// AllowsSyntheticIdentity reports whether a view-originated inbound on this
	// kind may resolve an email-less identity to a synthetic subject
	// (base64(kind:teamScope:externalID)). True ONLY for kinds whose users are
	// legitimately email-less — the `local` TUI on a no-IdP cluster, whose
	// OS-user identity IS synthetic by design. False for browser/Slack, whose
	// users always carry a verified email, so an email-less claim there is a
	// forgery attempt and must fail closed. Keyed off the SESSION's own
	// witnessed channel kind, never off the wire payload.
	AllowsSyntheticIdentity() bool

	// RelayedByChannelsd reports whether channelsd hosts this kind's
	// Listener/Sender/StreamDeltaSink. False for kinds channelsd hosts no
	// transport for — client-hosted kinds whose transport is the end-user's
	// own process (the `local` TUI, hosted by `oap`), and kinds received
	// instead via webhook (see WebhookReceiver); channelsd's listener-starter
	// and sender resolution skip those, since the transport cannot exist in a
	// channelsd pod.
	RelayedByChannelsd() bool

	// SpawnsSessionOnInbound reports whether an inbound may spawn a brand-new
	// AgentSession when no active session exists for the (channel, key) pair.
	//
	// True for durable channels hosting many conversations over their
	// lifetime (slack, fake, bento). False for kinds owning exactly ONE
	// session for the lifetime of their transport — the `local` TUI, where
	// `oap agent chat` pre-creates the single session it drives. For those, an
	// inbound with no active session MUST NOT spawn a phantom
	// `<channel>-<uuid>` session; the pipeline returns OutcomeNoActiveSession
	// so the transport can tell the user the interaction ended.
	SpawnsSessionOnInbound() bool

	// WebAuthenticator returns the kind's OIDC / "Sign in with X" provider,
	// or nil if the kind doesn't support web authentication. Used only by
	// identityd (passthrough self-service). The deps shape is whatever the
	// kind needs to construct its authenticator — for slack: the Slack
	// OAuth client_id + client_secret + the external redirect URL.
	WebAuthenticator(deps WebAuthDeps) WebAuthenticator

	// WebhookReceiver returns the kind's inbound-HTTP receiver, or nil if the
	// kind has no webhook surface. Mounted by webd's channelwebhook plug-in
	// via a registry sweep; webd knows no kind names.
	//
	// "nil" here MUST mean an untyped nil interface, never a typed-nil
	// pointer (`var r *someReceiver; return r`). channelsd's session-open
	// path (pkg/channels/channelsd/pipeline's Deliver) type-asserts this
	// return value to TriggerFactProvider and, on a successful assertion,
	// calls a method on it unconditionally — the classic Go gotcha (see
	// AGENTS.md's "Nil interfaces" rule) applies here exactly as it does to a
	// typed-nil assigned into an interface field: a typed-nil pointer read
	// back out of an interface is never `== nil`, so the assertion would
	// succeed and the call would panic on a nil receiver. Return a literal
	// `nil` (as every registered kind does today) or a zero-size value type
	// (github's receiver{}), never a possibly-nil pointer.
	WebhookReceiver(deps Deps) WebhookReceiver

	Wizard() Wizard
}

type Listener interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// SessionWatcher is an OPTIONAL interface a Listener implements to react to
// AgentSession changes naming this Channel as the OutputChannel. channelsd
// subscribes to channelevents.SessionAttachedSubject and invokes
// SessionUpdated for every event whose OutputChannelName matches. Slack uses
// it to learn a cron-spawned session's thread_ts so in-thread replies pass the
// threadIndex gate.
//
// Implementations MUST be idempotent — the same session may be reported
// several times per channelsd uptime — and safe for concurrent invocation.
type SessionWatcher interface {
	SessionUpdated(ctx context.Context, sess *spiceboxv1alpha1.AgentSession)
}

// ScopeRefresher is the OPTIONAL interface a Listener implements when the
// answer to "does this Channel have the permissions it needs?" can change while
// it is connected. Both sides of that answer can: the bound AgentClass's
// capabilities decide what is REQUIRED, and the third party decides what is
// GRANTED — re-installing a Slack app adds a scope to the same bot token, so
// nothing in the cluster changes and only the listener can notice. channelsd
// calls RefreshScopes on its reconcile tick (channelsd is not
// controller-runtime, so there is no watch to hang this off).
//
// ch is the Channel as of THIS tick, freshly listed. It carries the current
// spec — a listener's own Deps.Channel is its construction-time snapshot, and
// a running listener is never restarted for a spec edit, so a rebind to a
// different AgentClass would otherwise go unnoticed — and the current status,
// so the implementation can compare before writing.
//
// Implementations MUST be cheap enough to run every tick (~5s per Channel) and
// MUST NOT write unless ch actually disagrees with them. They MUST NOT memo
// "what I last wrote" to skip that comparison: another writer can revert it,
// and the memo would make the revert permanent.
//
// An implementation that needs to ask the third party what it grants MUST
// throttle that call to far slower than the tick AND take it off this
// goroutine: channelsd calls RefreshScopes synchronously, inside the
// per-Channel loop of its single reconcile loop, so an inline round-trip puts
// every other Channel's reconcile behind one slow response. Refresh the cache
// there and let a later tick recompute from it.
type ScopeRefresher interface {
	RefreshScopes(ctx context.Context, ch *spiceboxv1alpha1.Channel)
}

// SubChannelSendResult is what a Sender.Send returns. RequestRef is
// set by permission_request senders so the pipeline can stamp it on
// the AgentSession's PendingRequesters entry; other sub-channels
// leave it empty.
type SubChannelSendResult struct {
	// OpeningMessage identifies a summary message independently of the thread
	// routing anchor, so pinned updates never overwrite an existing thread root.
	OpeningMessage *MessageRef
	RequestRef     string

	// External, when non-empty, is patched back onto the AgentSession's
	// OutputChannel.External + OutputChannel.Key by channelsd's outbound
	// relay. Senders use this to record routing metadata captured by the
	// first send (slack's chat.postMessage returns the thread root's ts
	// when none was supplied) so subsequent sends — and inbound listeners
	// matching thread replies back to this session — can find the same
	// thread. Empty / nil means "no metadata to write back"; the relay
	// skips the patch.
	External map[string]string
}

// Sender delivers an outbound envelope on a given sub-channel.
// The default sub-channel is "message"; permission_request and any
// future ones are negotiated via Kind.SubChannelSender.
type Sender interface {
	Send(ctx context.Context, sess SessionInfo, env channelevents.Envelope) (SubChannelSendResult, error)
}

// DeliveryReceiverProvider is an optional durable-acceptance seam. Kinds whose
// only delivery path is best-effort leave it unimplemented; consumers fail
// closed rather than treating Sender.Send success as a transport receipt.
type DeliveryReceiverProvider interface {
	NewDeliveryReceiver(memory.Memory) delivery.Receiver
}

// Deps is what internal/cmd/channelsd hands every kind impl. Listener uses Inbound
// to deliver events; Sender ignores Inbound.
type Deps struct {
	Channel *spiceboxv1alpha1.Channel
	Secret  *corev1.Secret

	// NATSPublish is for one-off publishes if a kind needs to bypass the
	// pipeline. Most kinds won't use this; the shared inbound pipeline owns
	// the routine NATS publishes.
	NATSPublish func(subject string, payload []byte) error

	// NATSRequest is the request-reply transport for kinds whose "listener" is
	// a browser or TUI: they submit a user message to channelsd and need the
	// InboundDecision back synchronously to render a deny/notice. Wired by the
	// host process (webd, oap, the e2e harness). Nil is a wiring bug, surfaced
	// loudly by RequestViewMessage rather than nil-panicking.
	NATSRequest channelevents.RequestFunc

	// Inbound is the shared pipeline a Listener calls into when it receives
	// an external event.
	Inbound InboundPipeline

	K8sClient client.Client

	LogTopic func(format string, args ...any)

	// TouchSetStatus is called whenever the kind successfully emits a
	// setStatus (or equivalent) to its transport. The channelsd watchdog uses
	// these touches to decide when an agent has gone silent too long.
	// Optional; nil-safe.
	TouchSetStatus func(ns, name string)

	// ForgetSetStatus drops the watchdog's tracking for a session — used
	// after a final user-facing message has been delivered (the user has
	// their answer, so no silence alarm should fire). Optional; nil-safe.
	ForgetSetStatus func(ns, name string)

	// ExtendSetStatus pushes the watchdog's silence deadline forward by dur,
	// so a declared long-running operation (update_status's
	// expected_duration_seconds) doesn't trip the "taking longer than
	// expected" warning. channelsd caps the honored value at 5 minutes.
	// Optional; nil-safe.
	ExtendSetStatus func(ns, name string, dur time.Duration)

	// AssetFetcher resolves an OutboundUserMessagePayload.Attachments[i] ref
	// to its bytes at delivery time. An interface so this package stays free
	// of channelsd dependencies. Optional; nil leaves the text-only path.
	AssetFetcher AssetFetcher

	// StreamDeltaSink renders runner-emitted LLM stream events onto the
	// channel's UI surface. Nil = the kind opts out (no live stream
	// rendering on this channel). Generic channelsd code routes events
	// to the sink; the sink owns debounce / throttle policy.
	StreamDeltaSink StreamDeltaSink

	// AuthzReader gives sub-channel senders read-only SpiceDB access — e.g.
	// fanning an approval ephemeral out to every member of the configured
	// ApproverSubject set. Optional; implemented in channelsd so this package
	// stays free of SpiceDB transport dependencies.
	AuthzReader AuthzReader

	// PortalLinkMinter mints a portal-purpose signed link for a canonical
	// subject, deep-linking "Manage my connections" into identityd's
	// /my/accounts. Optional: nil (no signer configured) means the kind omits
	// the link rather than failing.
	PortalLinkMinter PortalLinkMinter

	// ExternalBaseURL returns identityd's externally reachable base URL, used
	// to compose per-credential icon URLs. A getter, not a string, so the URL
	// can change at runtime (tunnel rotation) without restarting the
	// listener. Nil or an empty return omits the icons.
	ExternalBaseURL func() string

	// Memory gives listeners read access to session memory: the "restart from
	// here" shortcut reads channel_msg_ref entries, and Show Details
	// re-renders approval details from persisted records when the in-process
	// cache misses. Optional — nil degrades those handlers to an error modal.
	Memory memory.Memory

	// Preferences gives channel kinds first-party access to the operator's
	// preferences endpoints — a verified human reading/editing THEIR OWN
	// preferences via a first-party surface (the Slack App Home). Optional —
	// nil degrades the App Home preferences pane to a "couldn't load" state.
	Preferences PreferencesClient

	// PersonalizableClasses answers "which AgentClasses has this canonical
	// user interacted with" — the App Home preferences section (Task 11)
	// uses it to decide which class cards get a preferences section at all,
	// rather than probing every class's preferences endpoint speculatively.
	// Optional — nil (or an error at call time) degrades App Home to render
	// every class's manage-connections card with no preferences section,
	// same as a user who has interacted with nothing.
	PersonalizableClasses PersonalizableClassLookup

	// ArtifactViewMinter mints the signed artifact-view deep-link the
	// live_view_offer sender posts. Nil means webd is not configured; the
	// sender logs and skips rather than failing.
	ArtifactViewMinter ArtifactViewMinter

	// SessionViewMinter composes the durable session-view page URL the
	// session_view_offer sender posts. Nil means webd is not configured; the
	// sender logs and skips. Unlike ArtifactViewMinter this carries NO signed
	// capability — the page enforces its own CheckInteract at open time — so
	// the link is durable and safe to bake in at post time.
	SessionViewMinter SessionViewMinter

	// AgentUIMinter composes the agent-UI shell page URL the agent_ui_offer
	// sender posts. Nil means webd is not configured. Declared as the
	// interface type so the zero value is a true nil interface — a typed-nil
	// pointer here would make every sender's nil check lie.
	AgentUIMinter AgentUIMinter

	// ApproverFanoutLimit caps how many resolved approvers one approval prompt
	// notifies (after union + dedupe across the request's subject-sets). It
	// bounds notification noise only — it does NOT bound who may approve:
	// click-time authorization admits any eligible approver, notified or not.
	// 0 ⇒ DefaultApproverFanoutLimit. Set from channelsd's
	// --approver-fanout-limit.
	ApproverFanoutLimit int
}

// DefaultApproverFanoutLimit is the default for Deps.ApproverFanoutLimit:
// how many approvers a single approval request notifies when the eligible
// set is larger. Delivery-only; never an authorization bound.
const DefaultApproverFanoutLimit = 10

// ResolvedApproverFanoutLimit returns the effective fan-out cap (the
// configured value, or DefaultApproverFanoutLimit when unset/invalid).
func (d Deps) ResolvedApproverFanoutLimit() int {
	if d.ApproverFanoutLimit > 0 {
		return d.ApproverFanoutLimit
	}
	return DefaultApproverFanoutLimit
}

// PreferencesClient gives channel kinds first-party access to the operator's
// preferences endpoints — a verified human reading/editing THEIR OWN
// preferences via a first-party surface (the Slack App Home). nil degrades
// the App Home preferences pane to a "couldn't load" state. Backed by the
// operator memory httpclient's first-party methods (*httpclient.Client
// satisfies this structurally); wired in internal/cmd/channelsd from the
// same client the memory facade already holds.
type PreferencesClient interface {
	GetPreferencesFirstParty(ctx context.Context, ns, className, subject string) (preferences.SnapshotResponse, error)
	CommitPreferenceFirstParty(ctx context.Context, ns, className string, req preferences.CommitRequest) error
}

// PersonalizableClassLookup gives channel kinds a narrow, read-only way to
// enumerate "which AgentClasses has this canonical user interacted with" —
// the SpiceDB agentclass#can_personalize permission a message/tool-call from
// that user grants (see TouchInteractor). The App Home preferences section
// (Task 11) uses this to decide which classes get a preferences card,
// without walking every AgentClass's tuples itself.
//
// Implemented by a thin adapter over *pkg/authz/spicedb.Client's
// LookupPersonalizableClasses (see internal/cmd/channelsd/main.go), returning
// plain "<namespace>/<class>" strings rather than a spicedb-package type —
// this keeps channelkinds free of the SpiceDB transport dependency, the same
// discipline AuthzReader documents above.
type PersonalizableClassLookup interface {
	// LookupPersonalizableClassRefs returns "<namespace>/<class>" refs for
	// every AgentClass canonicalID holds can_personalize on, capped at
	// limit. This is a rendering hint, not a security gate, so callers
	// should pass fullyConsistent=false.
	LookupPersonalizableClassRefs(ctx context.Context, canonicalID identity.CanonicalUserID, limit uint32, fullyConsistent bool) ([]string, error)
}

// PortalLinkMinter mints "<base>/my/accounts?d=&sig=" portal-purpose signed
// links, so sub-channel senders can deep-link into identityd's portal without
// holding the signing key. The minter derives Subject and SubjectVerified from
// the Principal, so the verified flag can only originate from the identity
// authority.
type PortalLinkMinter interface {
	MintPortalLink(ctx context.Context, subject identity.Principal) (string, error)
}

// ArtifactViewMinter mints "<webd-base>/artifact-view?d=…&sig=…" signed
// deep-links, so the live_view_offer sender can compose the URL button without
// holding the signing key.
type ArtifactViewMinter interface {
	// MintArtifactViewLink mints a webd artifact-view deep-link. The minter
	// derives Subject and SubjectVerified from subject, so the verified flag
	// can only originate from the identity authority. backLink is optional and
	// opaque — the web UI renders it as a "back to origin" link; pass "" when
	// the kind has no permalink concept.
	MintArtifactViewLink(artifactID, sessionRef string, subject identity.Principal, backLink string) (string, error)
}

// SessionViewMinter composes "<webd-base>/session-view/<ns>/<name>" durable
// plain-path links for the session_view_offer sender. Unlike
// ArtifactViewMinter the URL carries NO signed capability and NO TTL:
// authorization happens at open time via the page's own CheckInteract.
type SessionViewMinter interface {
	// MintSessionViewLink returns the session-view page URL for sessionRef
	// ("<ns>/<name>"). subject and backLink are accepted for parity with
	// ArtifactViewMinter but are NOT embedded in the URL — they have no
	// bearing on who may open it.
	MintSessionViewLink(sessionRef string, subject identity.Principal, backLink string) (string, error)
}

// AuthzReader exposes the read-only SpiceDB operations a channel-kind
// sub-channel sender may need. Implemented by *pkg/authz/spicedb.Client.
type AuthzReader interface {
	// LookupSubjects expands the subject-set expression
	// "<objType>:<objID>#<relation>" into a list of canonical user IDs
	// (NOT prefixed with "user:" — callers re-prefix if needed).
	LookupSubjects(ctx context.Context, subjectRef string) ([]string, error)

	// CheckInteract reports whether canonical (the "user:..." subject)
	// has interact permission on agentsession:<ns>/<name>. Used by the
	// restart-from-here shortcut to gate the modal-open on the
	// invoking user's permission. fullyConsistent=true so a freshly-
	// joined participant is reflected immediately.
	CheckInteract(ctx context.Context, ns, name string, canonical identity.CanonicalUserID, fullyConsistent bool) (bool, error)
}

// Capability ranks how completely a channel kind can resolve an audience
// for information-leakage gating. Higher is "more capable". Kinds without
// an AudienceResolver implementation are treated as CapabilityUnsupported.
type Capability int

const (
	CapabilityUnsupported Capability = iota
	CapabilitySingleUser
	CapabilityFull
)

// AudienceResolver is an optional interface a Kind may implement. The runner
// uses it during the information-leakage gate at respond_to_user emission.
//
// AudienceCapability is a static property of the Kind and MUST NOT perform
// I/O — it is read at binding-time validation. ResolveAudience may perform
// SpiceDB lookups; it is called at gate time to enumerate the canonical
// subjects who would receive a message on this channel.
type AudienceResolver interface {
	AudienceCapability() Capability
	ResolveAudience(ctx context.Context, sess SessionInfo) ([]string, error)
}

// SchemaContributor is an optional interface a Kind may implement to declare
// the SpiceDB resource definitions it needs at gate/listener time. The
// operator collects fragments from all registered channel kinds and includes
// them in the composed schema alongside MCPServer fragments.
//
// Returning nil is equivalent to not implementing the interface — the kind
// contributes nothing. Implementations MUST return the same fragment across
// calls (read once at schema-write time, not per-session).
type SchemaContributor interface {
	SpiceDBSchemaFragment() *spiceboxv1alpha1.SpiceDBSchemaFragment
}

// SessionOwnerProvider is an optional Kind capability: given an inbound event,
// return the per-session starting owner subject (e.g. "user:<canonical>") and
// whether this kind provides one. Kinds with no human starter (bento) return
// ("", false). Consumers type-assert; not part of Kind.
type SessionOwnerProvider interface {
	SessionOwner(ev InboundEvent) (subject string, provides bool)
}

// OwnerGroupProvider is an optional Kind capability: return the channel's live
// membership subject-set ref (e.g. "slack_channel:<id>#member") for use as an
// owner source when this kind is the output channel, and whether one exists.
type OwnerGroupProvider interface {
	OwnerGroupRef(ch *spiceboxv1alpha1.Channel) (subjectSetRef string, ok bool)
}

// OutboundAnchorProvider is an optional Kind capability: seed the initial
// outbound routing metadata for a session whose outbound target is this
// Channel but whose inbound origin is a different one (cron / split-channel).
//
// The returned key and external become AgentSession.spec.outputChannel.Key and
// .External. Both MUST be pure functions of ch — no wall-clock, no randomness —
// so re-deriving the binding for the same Channel always yields identical
// values. The outbound relay refines the key to thread:<id>:<ts> on first send
// — which, for a session no human started, is the trigger's own opening line
// rather than the agent's first output (see TriggerDescriber).
//
// A non-nil error means the Channel cannot serve as a dedicated output target.
// The Channel controller surfaces that at apply time where it can — on the
// role=input Channel bound to the same class, and on this one when that class's
// input carries no human — and puts the error in the condition message, so the
// error MUST name the kind's own field a human has to fill in
// ("spec.slack.outputDefaults.channelId is required"), not merely report that
// something is absent. The
// kind is the only party that knows which field that is; a consumer that had to
// name it would be switching on kind.
type OutboundAnchorProvider interface {
	OutboundAnchor(ch *spiceboxv1alpha1.Channel) (key string, external map[string]string, err error)
}

// TriggerDescriber is an optional Kind capability: say in one line what an
// inbound of this kind is ABOUT — which pull request, which schedule — from a
// Channel, the binding key this kind itself produced, and (when the inbound
// was a webhook) the verified delivery that opened the session.
//
// It exists for the outbound side of a session whose inbound carries no human
// (registry.IsUserlessInput). Such a session has no message of its own to open
// its thread with, so without this the thread's first line is the agent's first
// output. The kind that WROTE the binding key is the only party that can read
// it back, which is why the anchor mechanism asks rather than parsing a key it
// does not own — see channelkinds/outputbind.SessionOpening.
//
// event and body are the delivery as it crossed NATS — the same (event, body)
// pair TriggerFactProvider and TriggerOwnerProvider read, and empty for an
// inbound that was not a webhook (a cron tick). They exist so the line can say
// what the triggering thing IS (a title, an author, a size), not merely which
// one it is. The KEY stays authoritative for identity: a kind must derive
// WHICH object the line names from channelKey alone, and treat the body as
// decoration it degrades gracefully without — never as a substitute for a key
// it cannot parse. Provider-authored text in the body (a title, a login) is
// untrusted and must be sanitized to one display-safe line before it is
// rendered; the line is posted verbatim onto the output surface.
//
// Pure function of (ch, channelKey, event, body): no wall-clock, no
// randomness, no I/O. The same trigger must always render the same line.
//
// ok=false means "I cannot describe this trigger" — a key this kind did not
// write, or a Channel missing the field the sentence needs. The caller then
// posts nothing at all: a thread rooted by the agent's first output is the
// behaviour every kind has today, and an empty or placeholder opening is worse
// than none.
type TriggerDescriber interface {
	DescribeTrigger(ch *spiceboxv1alpha1.Channel, channelKey, event string, body []byte) (text string, ok bool)
}

// MessageRef identifies one already-posted message on a channel's transport,
// in that transport's own terms. Slack: ChannelID is the channel/conversation
// id, TS is the message timestamp chat.update needs. Opaque to callers
// outside the owning kind.
type MessageRef struct{ ChannelID, TS string }

// OpeningMessageContent is the desired rendering of a session's live opening
// message: the same status a triggered session's thread-root line carries,
// re-rendered in place as the session progresses rather than appended below
// it. OpeningText is the original one-line description (see
// TriggerDescriber); Badge, Body, and Link are the evolving status — the
// current outcome marker, the supporting detail, and an optional deep link —
// as of the edit call.
type OpeningMessageContent struct {
	Ref         MessageRef
	OpeningText string
	Badge       spiceboxv1alpha1.OpeningBadge
	Body        string
	Link        string
	// Instructions enables inspection of the exact initial prompt when the
	// opening is a summary. Editors must retain its inspection control.
	Instructions string
}

// OpeningMessageEditor is an OPTIONAL capability a Kind's Sender may
// implement when its transport can edit an already-posted message in place
// (Slack's chat.update). Consumers type-assert the resolved Sender and skip
// the live-status update when it is absent, falling back to whatever posting
// behavior the kind already has.
type OpeningMessageEditor interface {
	EditOpeningMessage(ctx context.Context, sess SessionInfo, content OpeningMessageContent) error
}

// SessionRelationLinker is an optional Kind capability: declare the subject-set
// link-types this kind permits on agentsession#owner / #participant. The
// guardian composer unions these (plus user) into the agentsession definition.
// A kind MUST also contribute (via SchemaContributor) the definitions these
// link-types reference.
type SessionRelationLinker interface {
	SessionRelationLinks() []string
}

// AssetFetcher resolves an attachment ref (ns, sess, render) to its bytes.
// Implementations live outside pkg/channels/channelkinds (internal/cmd/channelsd) so this
// package stays a leaf with no operator-wiring dependencies.
type AssetFetcher interface {
	Fetch(ctx context.Context, ns, sess, render string) (AssetBytes, error)
}

// StreamDeltaSink renders runner-emitted LLM stream events onto the
// channel's UI surface. Implementations are channel-kind-specific and
// own their own debounce / rate-limit policy. Generic channelsd code
// routes events to the kind's sink without inspecting them.
//
// Implementations must be safe for concurrent calls across threads.
// Per-thread state lives inside the sink.
type StreamDeltaSink interface {
	OnDelta(ctx context.Context, sess SessionInfo, env channelevents.Envelope) error
}

// MonitoringSender delivers a framework MonitoringEvent to a
// role=monitoring Channel. Implementations are channel-kind-specific
// (slack renders mrkdwn; fake records to a buffer) and read their
// destination from the Deps.Channel they were constructed with.
// Implementations must be safe for concurrent calls.
type MonitoringSender interface {
	SendMonitoring(ctx context.Context, ev channelevents.MonitoringEvent) error
}

// AssetBytes is the resolved-attachment payload returned by an
// AssetFetcher.Fetch. Bytes is the full body; MIME and Filename are
// echoed from the operator's response headers and serve as hints to the
// kind-specific upload path (Slack files.uploadV2 etc.).
type AssetBytes struct {
	Bytes    []byte
	MIME     string
	Filename string
}

type SessionInfo struct {
	// Namespace and Name identify the AgentSession.
	Namespace string
	Name      string

	// SessionInitiator is the BARE canonical user id of the user who
	// started the session (identity.CanonicalUserID — the base64 body,
	// no "user:" prefix). Always set for SingleUser-capability resolvers;
	// may be empty for Full-capability resolvers that don't need it.
	SessionInitiator identity.CanonicalUserID

	// Channel is the bound channel metadata. Kind-specific routing data
	// (channel_id, team_id, thread_ts, etc.) is in Channel.External.
	Channel *spiceboxv1alpha1.ChannelBinding

	// Annotations is a copy of the AgentSession's metadata.annotations,
	// surfaced so channel kinds can read per-turn signals a kind stamps on the
	// session (e.g. Slack's last-inbound thread_ts) without a K8s Get.
	// Populated by the outbound relay; may be nil.
	Annotations map[string]string
}

// InboundPipeline is the channelsd-side shared inbound flow. Implemented
// by pkg/channels/channelsd/pipeline; Listener calls Deliver on each external event.
type InboundPipeline interface {
	Deliver(ctx context.Context, ev InboundEvent) (InboundDecision, error)
}

type InboundEvent struct {
	Channel       *spiceboxv1alpha1.Channel
	ExternalIDs   ExternalIdentity
	ChannelKey    string
	MessageText   string
	MessageBlocks json.RawMessage
	External      map[string]string
	Reply         InboundReplyHooks

	// ThreadEntry classifies how a thread-capable inbound entered its thread:
	// "" not a thread message (DM, non-thread kind), "root" the mention starts
	// the thread, "reply" it lands inside a pre-existing one. The listener
	// sets it from raw transport facts and decides nothing; the pipeline
	// combines it with session correlation to decide whether to adopt
	// (backfill) the thread.
	ThreadEntry string

	// FromAgent marks an inbound authored by ANOTHER agent rather than a
	// person. The listener sets it from raw transport facts and decides
	// nothing — the same contract as ThreadEntry — and the pipeline combines
	// it with the bound class's wake budget to decide whether this message
	// may also WAKE the session or only append to it.
	//
	// It is NOT an authorization input. Admission was already decided by the
	// listener; this decides only whether a person has to speak before the
	// agent answers again. That is why it is set at the same site that
	// classifies origin and nowhere further downstream: a false value here on
	// a genuine agent message is not a leak, it is the unbounded loop the
	// budget exists to stop.
	FromAgent bool

	// PreTurnAnnotations are session annotations that must be in place BEFORE
	// the turn this inbound starts, because the turn's own output depends on
	// them.
	//
	// Slack's last-inbound thread anchor is the motivating case, and the bug
	// is worth stating: the listener stamped it AFTER Deliver returned, but
	// Deliver had already appended the turn and woken the runner. Under load
	// the runner produced respond_to_user and the sender read the session
	// before the patch landed, found no anchor, and posted the reply
	// TOP-LEVEL — an orphan message in the user's DM rather than a reply under
	// their question. The result was wrong, not merely late, which is why the
	// fix is ordering rather than a longer wait.
	//
	// The pipeline writes these before it wakes anything. A kind that needs
	// nothing leaves it nil.
	PreTurnAnnotations map[string]string

	// RequesterCanonicalIDAnnotation names the annotation the resolved
	// canonical requester id is written to, in the SAME patch as
	// PreTurnAnnotations. The pipeline holds the value and the kind holds the
	// key, so neither has to import the other.
	//
	// It races the same way: the runner reads it at tool-dispatch time to
	// decide the authz subject. That path fails CLOSED where the sender's
	// failed open, so it never produced a wrong Slack post — but it is the
	// same missing write, and moving both together is what stops the pair
	// drifting apart later.
	RequesterCanonicalIDAnnotation string

	// WakeBudget is the bound class's declared budget of agent-driven wakes
	// per human turn, carried from the listener because the listener already
	// resolves the class on its reconcile tick. The pipeline reads no class
	// for this: doing so would be a GET per inbound to answer a question one
	// GET a minute already answers.
	//
	// Zero — the value every kind that does not set it reports — means
	// agent-driven wakes are OFF, never unlimited, so a kind that knows
	// nothing about cross-agent participation cannot accidentally grant it.
	WakeBudget int

	// AuthzSubject overrides per-user SpiceDB subject resolution: it is the
	// ACTING SUBJECT of this inbound, used verbatim as the canonical for every
	// permission check downstream when it is non-empty AND no per-user
	// identity is supplied. It is NOT written as started_by — that relation is
	// user-typed, so a non-human subject suppresses the write.
	//
	// Where it comes from depends on the caller: a listener for a
	// non-user-attributable kind (bento) copies its Channel's
	// spec.authzSubject, and the AgentClass validator enforces the Channel
	// carries one. A bus handler supplies the subject the arriving NATS
	// subject authorized — HandleAgentMessageSend passes the SENDING session,
	// which is not what the pair Channel's own spec.authzSubject says.
	AuthzSubject string

	// Via is the view URN of the surface this inbound came from (session-view
	// messages only). Stamped onto the memory turn by Deliver. Empty for a
	// Channel's own listener.
	Via string

	// Attachments lists files the user attached, as kind-opaque references.
	// The listener populates this unconditionally whenever the transport
	// reports files — no capability check, no Channel spec lookup. Whether any
	// bytes are ever fetched is decided downstream, by whoever holds both an
	// AttachmentFetcher and a reason to call it.
	Attachments []InboundAttachment

	// RawDelivery is the verbatim provider payload for an inbound that arrived
	// as a signed webhook, and is EMPTY for a listener's own inbound.
	//
	// Carried rather than re-fetched because it cannot be re-fetched: the body
	// is what was signed, and a provider's API does not hand back the delivery
	// it sent. It is recorded into trigger_delivery once the session scope is
	// known, which is the first point downstream of here that has one.
	//
	// Left naturally empty for non-webhook kinds rather than stripped
	// explicitly — only the receive path sets it, so there is no branch to keep
	// correct.
	RawDelivery []byte

	// DeliveryEvent is the provider's event-type header value for that same
	// webhook. Named separately because the receiver dispatches on it and the
	// body does not carry it.
	DeliveryEvent string
	// TargetSession names the AgentSession this inbound is for, when the
	// caller already knows it authoritatively. When set, the pipeline
	// delivers into THAT session and skips the correlation lookup
	// (LabelChannelName + LabelChannelKey) it would otherwise run against
	// Channel. Leave it nil — the zero value — from a Listener.
	//
	// Correlation exists for a transport that knows only a channel-side key
	// (a Slack thread ts, a bento message) and has to discover which session
	// owns it. A bus handler serving ap.session.<ns>.<name>.in.* does not: the
	// session is in the SUBJECT, already cross-checked against the envelope
	// before the handler runs (internal/cmd/channelsd/main.go's
	// envelopeHandler). It also cannot always correlate, because the Channel
	// it delivers through need not be one the target is bound to — a
	// session-to-session conversation rides the single Channel provisioned for
	// the delegation edge, which is bound to the CHILD, so the correlation
	// labels on it resolve to the child no matter which end the message is
	// addressed to.
	//
	// Setting it widens no authorization. Every gate downstream is applied to
	// the session actually delivered into: the authzSubject type gate against
	// the Channel supplied alongside it, and the InboundTurn interact/converse
	// check against this session.
	TargetSession *client.ObjectKey
}

// MsgRef returns a kind-scoped opaque message ref for the channel_msg_ref
// memory kind. Empty means the External metadata lacks the keys needed to
// build one; callers skip writing the index entry (ref-indexing is
// best-effort, and the inbound still delivers).
//
// Format by kind:
//
//	slack: "<channel_id>:<thread_ts>:<message_ts>"
//	fake:  External["fake_ref"] (empty when absent)
func (e InboundEvent) MsgRef() string {
	switch e.ExternalIDs.Kind {
	case "slack":
		ch := e.External["channel_id"]
		ts := e.External["message_ts"]
		thread := e.External["thread_ts"]
		if ch == "" || ts == "" {
			return ""
		}
		return ch + ":" + thread + ":" + ts
	case "fake":
		return e.External["fake_ref"]
	default:
		return ""
	}
}

// ThreadEntry values for InboundEvent.ThreadEntry.
const (
	ThreadEntryRoot  = "root"
	ThreadEntryReply = "reply"
)

// OrgMembership classifies a channel-side user's standing in the org the
// channel is installed into, as attributed by the channel kind. It is an
// authorization input: the session-start gate treats anything but
// OrgMembershipMember as gated. A kind that attributes org membership (see
// Kind capabilities) must stamp member or guest on every identity it
// resolves — the empty value means "kind does not attribute membership" and
// is treated as guest by the gate when the kind claims to attribute it, so a
// dropped stamp fails closed rather than open.
type OrgMembership string

const (
	// OrgMembershipMember is a full member of the installed org/workspace.
	OrgMembershipMember OrgMembership = "member"
	// OrgMembershipGuest is anyone else the kind resolved: guests,
	// foreign-workspace users, bots, deleted accounts, and any identity
	// whose standing could not be determined.
	OrgMembershipGuest OrgMembership = "guest"
)

// ExternalIdentity is the channel-side identity of a user as resolved at
// runtime. ExternalID is always the raw, channel-native id — never a
// canonical user id. Use Principal() to derive the canonical identity.
type ExternalIdentity struct {
	Kind        identity.Kind // "slack", "fake"
	ExternalID  identity.RawExternalID
	Email       identity.Email     // optional
	TeamScope   identity.TeamScope // for fallback canonical_id; e.g., team_id
	DisplayName string             // optional; channel-native display/real name
	// OrgMembership is the kind-attributed org standing of this user; empty
	// when the kind does not attribute membership.
	OrgMembership OrgMembership
	// Subject, when set, is a pre-formed SpiceDB subject ("user:<id>")
	// that Principal() passes through verbatim via identity.RawSubject,
	// bypassing the kind/teamScope/externalID/email encoding entirely.
	Subject identity.Subject
}

// Principal derives the identity.Principal for this ExternalIdentity.
// When Subject is set it wins (RawSubject passthrough); otherwise the
// principal is derived from the raw channel-attributed fields via
// identity.FromExternal.
func (e ExternalIdentity) Principal() identity.Principal {
	if e.Subject != "" {
		return identity.RawSubject(e.Subject.String())
	}
	return identity.FromExternal(e.Kind, e.TeamScope, e.ExternalID, e.Email)
}

// FromPrincipal builds an ExternalIdentity from a channel-native Principal.
// DisplayName is not part of Principal and is left empty. It does NOT preserve
// a RawSubject passthrough — a Principal built via identity.RawSubject has no
// accessor for its subject — so FromPrincipal(e.Principal()) is NOT
// round-trip-safe for a Subject-passthrough ExternalIdentity. For those,
// construct ExternalIdentity{Subject: …} directly.
func FromPrincipal(p identity.Principal) ExternalIdentity {
	return ExternalIdentity{Kind: p.Kind(), TeamScope: p.TeamScope(), ExternalID: p.ExternalID(), Email: p.Email()}
}

// HasIdentity reports whether this identity is resolvable — either the natural
// raw form (Kind+ExternalID) or a Subject passthrough. The delivery/resolve
// gates must use this rather than testing ExternalID alone, since Principal()
// treats a bare Subject as authoritative.
func (e ExternalIdentity) HasIdentity() bool {
	return (e.Kind != "" && e.ExternalID != "") || e.Subject != ""
}

type InboundReplyHooks struct {
	// Ephemeral renders a "visible only to the would-be poster" message via
	// the kind's transport. Slack: chat.postEphemeral. Fake: appended to a
	// slice on the test fixture.
	Ephemeral func(ctx context.Context, message string) error
}

type Outcome int

const (
	OutcomeUnknown Outcome = iota
	OutcomeRouted
	OutcomeDeniedByPermission
	OutcomeInternalError

	// OutcomeNoActiveSession means the inbound found no active session
	// for the (channel, key) pair AND the channel kind does not spawn a
	// new session on inbound (Kind.SpawnsSessionOnInbound() == false —
	// the `local` TUI kind). The message was deliberately NOT delivered
	// and NO session was created. This is a clean, explicit result: it
	// is NOT an internal error and NOT a permission denial — the
	// session simply ended and the transport should tell the user so.
	OutcomeNoActiveSession

	// OutcomeForkPending means the correlated session reached a terminal
	// phase a clean retry can continue from, so the pipeline wrote an
	// inherit fork-trigger on it: the operator will materialize a fresh
	// session that inherits the transcript and continues in a new thread.
	// The message was NOT routed into the terminal session (it has no
	// runner). Listeners post the Notice and must NOT set a "starting…"
	// status on the terminal thread.
	OutcomeForkPending

	// OutcomeRefused means the correlated session failed in a way a plain
	// retry cannot fix (budget exhausted, a policy halt, a scope-review
	// failure, a crashed runner), so no fresh session is spawned. Listeners
	// post the loud Notice naming why the session ended, and must NOT set a
	// "starting…" status (there is no runner).
	OutcomeRefused

	// OutcomeHandledNoAgent means channelsd consumed the inbound itself — a
	// sub-channel handler owned it (the portal-access trigger phrase) — so no
	// agent turn was started and none will be. The message was deliberately NOT
	// routed. Listeners post the Notice (which may be empty on the success path,
	// where the handler already delivered its own reply) and must NOT set a
	// "starting…" status: there is no runner behind it, and a placeholder with
	// nothing behind it strands the user until the silence watchdog fires.
	OutcomeHandledNoAgent

	// OutcomeThreadOwnedByAnotherAgent means the inbound landed on a thread
	// already bound 1:1 to a DIFFERENT agent, and this transport was not
	// summoned into it. Nothing was routed and no session was created.
	//
	// Normal, not an error: several agents' transports can be members of one
	// conversation and each receives its own copy of every event. The owning
	// agent is handling this same message, so the human is NOT waiting on the
	// refusing transport — it posts no Notice and sets no "starting…" status.
	//
	// Distinct from OutcomeHandledNoAgent, which a transport may legitimately
	// receive for a thread it DOES own. A transport that eagerly registered
	// the thread before delivering must withdraw that registration on THIS
	// outcome only.
	OutcomeThreadOwnedByAnotherAgent

	// outcomeSentinel is NOT an outcome. It marks the end of the declared set
	// so TestEveryOutcomeHasADistinctLabelAndRoundTrips can iterate every
	// member without a hardcoded bound. ALWAYS KEEP IT LAST: a member added
	// above this line is then automatically covered, and the test fails until
	// it is given a wire label in outcomeNames.
	outcomeSentinel
)

type InboundDecision struct {
	Outcome Outcome
	Session SessionInfo

	// Notice is the user-facing message for this decision: a refusal, a
	// lifecycle event, an error — whatever this outcome needs the user to
	// know. Nil means there is nothing to say.
	//
	// A refusal and a lifecycle message are the same kind of thing (one
	// message, one tone, one audience), so they share one field. What differs
	// is the DELIVERY surface, and that choice belongs to the channel kind —
	// the only layer that knows what surfaces it has.
	//
	// A DELIBERATE silence is notice.Suppressed(reason), distinguishable from
	// nil and carrying a reason for the log. Keeping "nothing to say" and "say
	// nothing on purpose" distinct is what makes a missing message detectable.
	Notice *notice.Notice

	// NewSession is true when the pipeline created a fresh AgentSession for
	// this inbound. Kinds gate one-time setup on it — the Slack listener posts
	// a thread-root placeholder only on new sessions; replies in an
	// established thread reuse the root a prior agent reply created.
	NewSession bool

	// RequesterCanonicalID is the canonicalized user ID of the inbound sender,
	// populated on OutcomeRouted so listeners can stamp it as a session
	// annotation for downstream tool-dispatch authz Checks. Empty means it was
	// unavailable (kubectl-driven sessions, permission-denied outcomes).
	RequesterCanonicalID string

	// PreTurnAnnotationsStamped reports that the pipeline already wrote
	// InboundEvent.PreTurnAnnotations and the canonical-id annotation, before
	// waking anything. A kind that stamps them itself as a fallback MUST skip
	// that write when this is true — otherwise every appended inbound costs
	// two patches of the same object with the same values.
	//
	// False on paths that route without starting a turn (live interactive-tool
	// input), where there is no wake to get ahead of and the kind's own
	// post-Deliver stamp is still the only writer.
	PreTurnAnnotationsStamped bool

	// Adopted is true when NewSession is true AND the pipeline backfilled a
	// pre-existing thread for this session. Listeners use it to post a
	// one-time join notice into the thread.
	Adopted bool

	// GrantedParticipants holds the display names of the thread participants
	// the pipeline auto-granted interact to during adoption, for the join
	// notice. Populated only when Adopted is true.
	GrantedParticipants []string

	// Queued is true when the inbound was appended behind an in-flight turn
	// rather than starting one. The pipeline already acked it on the
	// requester's sub-channel with an interrupt offer, so a kind that also
	// announced a start would contradict that ack AND overwrite the live
	// turn's progress caption. Kinds use this to leave the in-flight turn's
	// surface alone; the message is picked up when that turn ends.
	Queued bool

	// WithheldParticipants holds the display names of thread participants
	// adoption did NOT grant interact to, because the AgentClass declares an
	// interact policy that does not admit them.
	//
	// Reported rather than dropped: from inside the thread a withheld author is
	// indistinguishable from a granted one until they try to talk to the agent
	// and are ignored. The join notice names the restriction so the silence is
	// explained at the moment it is created.
	WithheldParticipants []string

	// Ownership describes WHO owns the newly-created session, so a kind can say
	// it out loud. Populated on the create path; zero otherwise.
	Ownership SessionOwnership
}

// SessionOwnership is the resolved answer to "who owns this session", in the
// form a channel needs to disclose it.
//
// Carried on the decision rather than re-derived by each kind because the
// precedence that produces it is subtle in a way that makes guessing dangerous:
// a starting user outranks both ownerless sources, so the SAME channel config
// yields an individual owner on a kind that attributes messages to a user and a
// whole-population owner on one that does not. A kind reading the config flag
// would confidently tell a room "everyone here owns me" while the operator had
// written a single person as owner.
type SessionOwnership struct {
	// Subject is the SpiceDB subject written to agentsession#owner.
	Subject string

	// Collective is true when Subject names a POPULATION rather than one
	// person — the case worth stating unprompted, because those people never
	// individually agreed to it and cannot see the tuple.
	Collective bool
}

// ScopesFor returns the union of the scopes k needs for the given features,
// deduplicated and sorted. Features k does not support contribute nothing.
//
// Sorted output matters: the generated app manifest is compared byte-for-byte
// across runs, and map iteration order would make it churn.
func ScopesFor(k Kind, features []channelfeatures.Feature) []string {
	sup := k.FeatureSupport()
	if len(sup) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range features {
		req, ok := sup[f]
		if !ok {
			continue
		}
		for _, s := range req.Scopes {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}
