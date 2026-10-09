// Package slack is the Slack channel-kind implementation.
//
// Inbound transport: Socket Mode (no public ingress required for v1).
// Outbound transport: Slack Web API (chat.postMessage, chat.postEphemeral,
// conversations.open).
//
// Capabilities: text + markdown (Slack mrkdwn), plus structured "plan"
// rendering via Block Kit PlanBlock + TaskCardBlock primitives for
// KindPlanUpdate envelopes.
//
// Required Secret keys: bot-token (xoxb-...), app-token (xapp-...).
package slack

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/appprovision"
	"github.com/authzed/openagentprimitives/pkg/cli/clikit"
)

const (
	KindName = "slack"

	SecretKeyBotToken = "bot-token"
	SecretKeyAppToken = "app-token"
)

func init() { registry.Register(&Kind{}) }

// Compile-time assertion: Slack supports conversation backfill.
var _ channelkinds.ConversationReader = (*Kind)(nil)

// Kind is the registered Slack channel-kind. It carries lazy-init
// resolver state (identity cache, client factory) used by LookupUser.
// All fields are guarded internally; the value is shared across
// goroutines within a process.
type Kind struct {
	resolverOnce  sync.Once
	identityCache *IdentityCache
	memberCache   *memberSnapshot
	// clientFactory builds a slackClient from LookupDeps. Override in
	// tests via the package-private setter (see lookup_test.go).
	clientFactory func(deps channelkinds.LookupDeps) slackClient

	// toolSessionRefsOnce + toolSessionRefs back a process-wide cache of
	// in-flight tool-session transcripts (interactive-mode ToolCalls).
	// Lazily initialised; only allocates when an interactive session runs.
	toolSessionRefsOnce sync.Once
	toolSessionRefs     *toolSessionRefCache

	// toolSessionSeenOnce + toolSessionSeen back a process-wide set of
	// sessions that have streamed tool output. The message sender reads
	// it to decide whether the final reply edits the placeholder or is
	// appended below the tool-session messages.
	toolSessionSeenOnce sync.Once
	toolSessionSeen     *toolSessionSeen

	// settingsButtonSeenOnce + settingsButtonSeen back the process-wide
	// set of sessions whose first agent message already carried the
	// "Show settings" button. Per-process (like the other shared caches):
	// a channelsd restart may show the button one extra time — accepted.
	settingsButtonSeenOnce sync.Once
	settingsButtonSeen     *toolSessionSeen

	// audience resolves the set of users who would receive a message on
	// a given Slack channel, used at respond_to_user gate time.
	// Wired via SetAudienceResolver; nil before wiring.
	audience *audienceResolver

	// restartSubmit is the channelsd-provided callback the listener
	// invokes when a user submits the Restart-from-here modal. Set
	// via RegisterRestartTrigger (channelkinds.RestartCapable) at
	// channelsd start-up; nil disables the submit path (the message
	// shortcut still opens the modal but submission returns an
	// "unconfigured" error).
	restartSubmit channelkinds.RestartSubmitFunc

	// metaagentBotUserID is the Slack user-id of the metaagent bot
	// (authzd), as wired explicitly by a binary. Set via
	// SetMetaagentBotUserID at channelsd start-up. Read through
	// metaagentBotUser(), NOT directly: a binary that wires no channel
	// kinds (the operator) leaves this empty and falls back to the
	// process environment. Empty from BOTH sources disables metaagent
	// mention routing entirely and makes MetaagentMembership report
	// MetaagentAppNotInstalled.
	metaagentBotUserID string

	// metaagentApprovalRefsOnce + metaagentApprovalRefs back the
	// process-wide cache of in-flight metaagent scope-approval
	// requests, shared between the sender (writes) and the listener's
	// Show Details handler (reads). Same lazy-init pattern as
	// toolApprovalRefs.
	metaagentApprovalRefsOnce sync.Once
	metaagentApprovalRefs     *metaagentApprovalRefCache

	// starterCacheOnce + starterCacheVal back a process-wide cache of
	// "started" bootstrap message coordinates, written by the listener's
	// postStarter and read by the thread_title sender to edit the starter
	// in place when emulating a thread title. Lazily initialised so
	// channels that never post a starter don't allocate the map.
	starterCacheOnce sync.Once
	starterCacheVal  *starterCache

	// forkRootOnce + forkRootVal back the process-wide cache that creates a
	// restart-fork child's thread root exactly once, no matter which sender
	// (slackSender's agent reply or toolSessionSender's streaming bubble)
	// reaches the channel first. Shared the same way toolSessionSeen is —
	// see the Kind type comment.
	forkRootOnce sync.Once
	forkRootVal  *forkRootCache

	// interactionDeliveryOnce + interactionDeliveryVal back the process-wide
	// shared interactionDeliveryStore (interaction_delivery.go): the SAME
	// instance is handed to the "interaction" sub-channel sender (writes
	// prompt/publicNote/Details at request-delivery time) AND the listener
	// (reads Details for the Show-Details modal) — the same sender/listener
	// sharing shape as authzRefs above. Lazily initialised so plain channel
	// kinds that never render an interaction envelope don't allocate the map.
	interactionDeliveryOnce sync.Once
	interactionDeliveryVal  *interactionDeliveryStore

	// installedTeamMu guards installedTeamID, the team ID of the workspace
	// this bot token belongs to, resolved via auth.test on first profile
	// lookup and cached ONLY on success. The listener learns the same value
	// at Start; the runner has no listener, so the Kind resolves it itself —
	// one notion of "our workspace" either way. Empty means unresolved (never
	// yet succeeded, or the last attempt failed), and the profile path fails
	// closed on it. A mutex rather than sync.Once so a transient auth.test
	// failure (rate limit, network blip) is retried on the next lookup
	// instead of permanently disabling the profile capability for the rest
	// of the process's life.
	installedTeamMu sync.Mutex
	installedTeamID string
}

// RegisterRestartTrigger implements channelkinds.RestartCapable. Called
// once by channelsd at start-up with the submit callback the Slack
// view_submission handler should call after parsing the modal. The
// listener reads this through Kind.restartSubmit at NewListener time.
func (k *Kind) RegisterRestartTrigger(submit channelkinds.RestartSubmitFunc) {
	k.restartSubmit = submit
}

// SetMetaagentBotUserID configures the Slack user-id of the metaagent bot
// (authzd). Called once at channelsd start-up from METAAGENT_SLACK_BOT_USER_ID.
// An empty string clears the explicit value and lets metaagentBotUser fall
// back to the process environment. The value is propagated to every new
// listener at NewListener time.
func (k *Kind) SetMetaagentBotUserID(id string) {
	k.metaagentBotUserID = id
}

// metaagentBotUser resolves the metaagent bot's Slack user-id: an explicitly
// wired value wins, otherwise the process environment.
//
// Two binaries need this answer and only one of them wires channel kinds.
// channelsd calls SetMetaagentBotUserID at start-up — from
// METAAGENT_SLACK_BOT_USER_ID, the very variable read here. The operator
// constructs no kinds at all: it reaches this Kind only through the registry,
// to ask MetaagentMembership, so the environment is its only route. Both
// binaries' Deployments carry the variable, sourced from the same
// agentprimitives-system-metaagent-config Secret.
//
// Read on each call rather than cached: the value is process configuration
// that arrives via env, and caching it would make the answer depend on when
// the Kind was first touched.
func (k *Kind) metaagentBotUser() string {
	if k.metaagentBotUserID != "" {
		return k.metaagentBotUserID
	}
	return os.Getenv(clikit.EnvMetaagentSlackBotUserID)
}

// sharedToolSessionRefs returns the per-process shared tool_session ref
// cache. Multiple deltas for the same ToolCall need to edit the same Slack
// message, so the cache must outlive a single Send call.
func (k *Kind) sharedToolSessionRefs() *toolSessionRefCache {
	k.toolSessionRefsOnce.Do(func() {
		k.toolSessionRefs = newToolSessionRefCache()
	})
	return k.toolSessionRefs
}

// sharedToolSessionSeen returns the per-process shared set of sessions
// that have streamed tool output. See the Kind type comment.
func (k *Kind) sharedToolSessionSeen() *toolSessionSeen {
	k.toolSessionSeenOnce.Do(func() {
		k.toolSessionSeen = newToolSessionSeen()
	})
	return k.toolSessionSeen
}

// sharedSettingsButtonSeen lazily initializes the once-per-session
// "Show settings" button tracker shared by every message sender this
// Kind creates.
func (k *Kind) sharedSettingsButtonSeen() *toolSessionSeen {
	k.settingsButtonSeenOnce.Do(func() {
		k.settingsButtonSeen = newToolSessionSeen()
	})
	return k.settingsButtonSeen
}

// sharedMetaagentApprovalRefs returns the per-process shared metaagent
// scope-approval ref cache. Lazily initialised so plain channel kinds
// that never receive a metaagent_scope_approval envelope don't allocate
// the map.
func (k *Kind) sharedMetaagentApprovalRefs() *metaagentApprovalRefCache {
	k.metaagentApprovalRefsOnce.Do(func() {
		k.metaagentApprovalRefs = newMetaagentApprovalRefCache()
	})
	return k.metaagentApprovalRefs
}

// sharedStarterCache returns the per-process shared starter-coordinate cache,
// written by the listener's postStarter and read by the thread_title sender.
func (k *Kind) sharedStarterCache() *starterCache {
	k.starterCacheOnce.Do(func() { k.starterCacheVal = newStarterCache() })
	return k.starterCacheVal
}

// sharedForkRoot returns the per-process shared fork-root cache. See the
// Kind type comment for the rationale.
func (k *Kind) sharedForkRoot() *forkRootCache {
	k.forkRootOnce.Do(func() { k.forkRootVal = newForkRootCache() })
	return k.forkRootVal
}

// sharedInteractionDelivery returns the per-process shared interaction
// delivery-record cache. Shared between the "interaction" sub-channel
// sender (writes prompt/publicNote/Details) and the listener's generic
// Show-Details handler (reads Details) — see the Kind type comment.
func (k *Kind) sharedInteractionDelivery() *interactionDeliveryStore {
	k.interactionDeliveryOnce.Do(func() { k.interactionDeliveryVal = newInteractionDeliveryStore() })
	return k.interactionDeliveryVal
}

func (*Kind) Name() string { return KindName }

// DefaultSessionScope returns "auto" — the listener interprets per-event
// (thread for #channels, user for DMs).
func (*Kind) DefaultSessionScope() string { return "auto" }

// Capabilities lists the format capabilities the runner uses to shape the
// respond_to_user JSON schema. Asset MIMEs are an explicit list (never an
// asset:image/* wildcard) so each delivered type is an audit-visible decision —
// image/svg+xml in particular is excluded from wildcard matching by the
// channelassets deny-list and must be opted into by exact MIME, as it is here.
// svg + css are bundled-only kinds: they are sanitized before delivery (svg by
// a hardened allowlist validator, css by the shared CSS denylist), and their
// in-browser preview is always shown inside the html-rendered live-view, never
// as a top-level document. Listing them here additionally permits attaching the
// sanitized file to a Slack message via respond_to_user.
func (*Kind) Capabilities() []string {
	return []string{
		"text",
		"markdown",
		"plan",
		"components",
		"asset:text/html",
		"asset:image/png",
		"asset:image/jpeg",
		"asset:image/gif",
		"asset:image/svg+xml",
		"asset:text/css",
		"asset:application/pdf",
	}
}

func (k *Kind) NewListener(deps channelkinds.Deps) channelkinds.Listener {
	l := newSlackListener(deps)
	l.metaagentApprovalRefs = k.sharedMetaagentApprovalRefs()
	l.starters = k.sharedStarterCache()
	l.restartSubmit = k.restartSubmit
	l.metaagentBotUserID = k.metaagentBotUser()
	l.interactionDelivery = k.sharedInteractionDelivery()
	return l
}

func (k *Kind) NewSender(deps channelkinds.Deps) channelkinds.Sender {
	s := newSlackSender(deps)
	s.toolSessionSeen = k.sharedToolSessionSeen()
	s.settingsSeen = k.sharedSettingsButtonSeen()
	s.forkRoot = k.sharedForkRoot()
	return s
}

// SubChannelSender returns the Sender for the named sub-channel:
//
//   - "message" — the standard message sender.
//   - "tool_session" — the interactive-session sender (KindToolSessionDelta:
//     non-terminal output chunks and the terminal exit delta).
//   - "live_view_offer" — renders an artifact live-view URL button pointing at
//     webd's /artifact-view endpoint.
//   - "session_view_offer" — the equivalent offer for a session view.
//   - "agent_ui_offer" — the equivalent offer for the agent-UI shell page
//     (a different page from session_view_offer's; the two buttons use
//     distinct copy so they don't read as duplicates when both land in the
//     same thread).
//   - KindThreadTitle — sets the Slack thread's title.
//   - KindUserEcho — the outbound mirror of a view-originated message
//     (channelsd's view_message pipeline), rendered into the session's own
//     thread with a real "<@U…>" mention resolved from
//     UserIdentity.status.channelIdentities (never wire display text).
//   - "interaction" — every prompt and notice in the unified Interaction model
//     (pkg/channels/channelevents/interaction.go, pkg/channels/channelinteractions): link
//     actions as Block Kit URL buttons, decision actions as decision buttons
//     encoded by buttoncodec.go, and each KindInteractionApplied outcome
//     edited in place. Slack has no per-category sub-channels — credential
//     linking, portal access, tool approval, info leakage, session-join
//     permission and the identity gate all arrive here.
//
// Every other name returns nil, and the outbound relay skips a sub-channel
// whose sender is nil. "queued_messages" is the one deliberate case: Slack's
// mid-turn enqueue ack rides the "interaction" sub-channel (category
// queued_messages) instead, while builtin/local/fake still map it to their own
// bespoke sender.
func (k *Kind) SubChannelSender(name string, deps channelkinds.Deps) channelkinds.Sender {
	switch name {
	case "message":
		s := newSlackSender(deps)
		s.toolSessionSeen = k.sharedToolSessionSeen()
		s.settingsSeen = k.sharedSettingsButtonSeen()
		s.forkRoot = k.sharedForkRoot()
		return s
	case "tool_session":
		s := newToolSessionSender(deps)
		s.refs = k.sharedToolSessionRefs()
		s.seen = k.sharedToolSessionSeen()
		s.forkRoot = k.sharedForkRoot()
		return s
	case "live_view_offer":
		return newLiveViewOfferSender(deps)
	case "session_view_offer":
		return newSessionViewOfferSender(deps)
	case channelkinds.SubChannelAgentUIOffer:
		return newAgentUIOfferSender(deps)
	case string(channelevents.KindThreadTitle):
		s := newThreadTitleSender(deps)
		s.starters = k.sharedStarterCache()
		return s
	case string(channelevents.KindUserEcho):
		return newUserEchoSender(deps)
	case "interaction":
		s := newInteractionSender(deps)
		s.delivery = k.sharedInteractionDelivery()
		return s
	case channelkinds.SubChannelMetaagentScopeApproval:
		return &metaagentScopeApprovalSender{
			client: newSlackAPIClient(deps.Secret),
			refs:   k.sharedMetaagentApprovalRefs(),
		}
	case channelkinds.SubChannelMetaagentNotice:
		return &metaagentNoticeSender{client: newSlackAPIClient(deps.Secret)}
	default:
		return nil
	}
}

// SupportedRoles is every role: slack both receives and sends, and it is the
// one kind with a MonitoringSender (SupportsMonitoring is true). Stated as the
// full set rather than nil — nil means "serves no role", which is not what
// this kind means. role=monitoring additionally needs
// spec.slack.outputDefaults.channelId, but that is a SPEC requirement checked
// in ValidateSpec, not a role this kind refuses.
func (*Kind) SupportedRoles() []string { return spiceboxv1alpha1.AllChannelRoles() }

// ValidateSpec rejects channels that mix slack with fake's spec
// block, and requires the slack config to be present. For monitoring
// channels it additionally requires spec.slack.outputDefaults.channelId.
func (*Kind) ValidateSpec(ch *spiceboxv1alpha1.Channel) error {
	if ch.Spec.Fake != nil {
		return fmt.Errorf(`spec.fake must be empty when kind="slack"`)
	}
	if ch.Spec.Slack == nil {
		return fmt.Errorf(`spec.slack is required when kind="slack"`)
	}
	if ch.Spec.Role == spiceboxv1alpha1.ChannelRoleMonitoring {
		if ch.Spec.Slack.OutputDefaults == nil || ch.Spec.Slack.OutputDefaults.ChannelID == "" {
			return fmt.Errorf(`spec.slack.outputDefaults.channelId is required when role="monitoring"`)
		}
	}
	return nil
}

// RequiredSecretKeys returns the secret keys required for the channel's role
// and mode. Monitoring channels are output-only (no socket listener), so they
// need only the bot token. Socket-mode (the only listener mode in v1) requires
// both bot-token and app-token.
func (*Kind) RequiredSecretKeys(ch *spiceboxv1alpha1.Channel) []string {
	if ch.Spec.Role == spiceboxv1alpha1.ChannelRoleMonitoring {
		return []string{SecretKeyBotToken}
	}
	mode := ""
	if ch.Spec.Slack != nil {
		mode = ch.Spec.Slack.Mode
	}
	if mode == "" || mode == "socket" {
		return []string{SecretKeyBotToken, SecretKeyAppToken}
	}
	return nil
}

// PublicSecretKeys is nil: every key this kind's Secret holds — the bot token,
// the app token, the signing secret — is credential material.
func (*Kind) PublicSecretKeys(*spiceboxv1alpha1.Channel) []string { return nil }

// RenderMention returns Slack's <@USERID> mention syntax. The
// generic pipeline uses this in deny / notification strings that
// need a clickable user handle without switching on kind.
func (*Kind) RenderMention(externalID string) string {
	if externalID == "" {
		return ""
	}
	return "<@" + externalID + ">"
}

// SupportedMentionLookups returns the set of identifier shapes this
// Slack kind can resolve. v1 supports all three (email, name, any).
// The implementations are in pkg/channels/channelkinds/slack/lookup.go.
func (*Kind) SupportedMentionLookups() []channelkinds.MentionLookupKind {
	return []channelkinds.MentionLookupKind{
		channelkinds.MentionLookupEmail,
		channelkinds.MentionLookupName,
		channelkinds.MentionLookupAny,
	}
}

// LookupUser delegates to lookup.go's implementation. Forwarded so the
// resolver state (HTTP client, caches) can live in a focused file.
func (k *Kind) LookupUser(
	ctx context.Context, deps channelkinds.LookupDeps,
	kind channelkinds.MentionLookupKind, value string,
) (string, string, error) {
	return k.lookupUser(ctx, deps, kind, value)
}

// MentionToolDescription returns the Slack-specific tool description.
// The text lives in lookup.go so it sits next to the resolution logic.
func (*Kind) MentionToolDescription() string { return slackMentionToolDescription }

func (*Kind) UserAttributable() bool { return true }

// DeliversToHuman is true: a Slack thread or DM is read by the people in it.
func (*Kind) DeliversToHuman() bool { return true }

// AllowsSyntheticIdentity: false. Slack users are email-verified (LookupUser
// resolves via Slack's users.info, which always carries a proven email), so
// an email-less author claim on a slack-bound session is a forgery attempt
// and must fail closed.
func (*Kind) AllowsSyntheticIdentity() bool { return false }

// RelayedByChannelsd: slack's Socket-Mode listener and Web-API senders
// run in a channelsd pod. True.
func (*Kind) RelayedByChannelsd() bool { return true }

// SpawnsSessionOnInbound: a slack Channel is durable and hosts many
// conversations over time — a message in a fresh thread starts a new
// session. True.
func (*Kind) SpawnsSessionOnInbound() bool { return true }

// AttributesOrgMembership: the listener classifies every resolved user as a
// full installed-workspace member or a guest (orgMembershipOf), so the
// pipeline's session-start gate applies to every slack channel.
func (*Kind) AttributesOrgMembership(_ *spiceboxv1alpha1.Channel) bool { return true }

// NewStreamDeltaSink returns the slack StreamDeltaSink for live
// rendering of assistant stream events. Returns nil when the deps'
// Secret is missing or malformed — without a bot token the sink
// can't issue chat.update calls; the relay then drops
// KindAssistantStreamDelta for this Channel silently.
//
// ThreadRoot is wired to the same per-process forkRootCache the sender uses
// (sharedForkRoot) so a restart-fork child's stream bubble is keyed and
// posted under the SAME link-back root the sender later consults via
// ConsumeFinalTarget — ensure() is idempotent and cached per session, so
// calling it from both the sink and the sender is safe and posts the root
// only once.
func (k *Kind) NewStreamDeltaSink(deps channelkinds.Deps) channelkinds.StreamDeltaSink {
	updater := NewSlackUpdaterFromSecret(deps)
	if updater == nil {
		return nil
	}
	poster := NewSlackPosterFromSecret(deps)
	cli := newSlackAPIClient(deps.Secret)
	return NewStreamDeltaSink(StreamDeltaSinkConfig{
		Updater: updater,
		Poster:  poster,
		ThreadRoot: func(ctx context.Context, sess channelkinds.SessionInfo, channelID string) string {
			return k.sharedForkRoot().ensure(ctx, cli, deps.K8sClient, sess, channelID)
		},
	})
}

// SupportsMonitoring is true: NewMonitoringSender below returns a real sender.
func (*Kind) SupportsMonitoring() bool { return true }

// SupportsLiveViewOffer is true: SubChannelSender("live_view_offer", …) returns
// a sender that posts the "👁 View live" interaction button.
func (*Kind) SupportsLiveViewOffer() bool { return true }

// NewMonitoringSender returns the slack MonitoringSender, which posts
// MonitoringEvents to the monitoring Channel's configured Slack channel.
func (*Kind) NewMonitoringSender(deps channelkinds.Deps) channelkinds.MonitoringSender {
	return newMonitoringSender(deps)
}

// WebAuthenticator returns the Slack OIDC authenticator. It implements
// "Sign in with Slack" (OpenID Connect): Begin builds the authorize URL and
// Complete exchanges the code for an id_token and extracts the email claim.
func (*Kind) WebAuthenticator(deps channelkinds.WebAuthDeps) channelkinds.WebAuthenticator {
	return newSlackAuth(deps, nil) // nil → default 30s-timeout client
}

// WebhookReceiver returns nil: this kind has no inbound HTTP surface — Slack
// deliveries arrive over Socket Mode / Events API, handled by NewListener.
func (*Kind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver { return nil }

// Wizard returns the Slack setup flow, wired to a real Slack client: the
// wizard verifies the bot token it is given by calling auth.test through this
// factory, and tests replace it with a stub.
//
// The timeout is not optional. slack-go's New installs a bare &http.Client{},
// which has none, and this call runs inside a form field's validator — on
// bubbletea's Update goroutine under the alt-screen driver, with the terminal
// in raw mode. A hung Slack API would therefore freeze the wizard with Ctrl+C
// queued behind the blocked Update and no way out but killing the process from
// another terminal, which leaves the terminal in raw mode. Same reason
// WebAuthenticator bounds its own client.
func (*Kind) Wizard() channelkinds.Wizard {
	return &slackWizard{
		authFactory: func(botToken string) authTester {
			return slackapi.New(botToken, slackapi.OptionHTTPClient(&http.Client{Timeout: wizardSlackTimeout}))
		},
		provisionClient: appprovision.NewHTTPClient(),
	}
}

// wizardSlackTimeout bounds the wizard's auth.test call. Long enough for a
// slow network, short enough that a user watching a frozen screen has not yet
// concluded the tool is broken.
const wizardSlackTimeout = 15 * time.Second

// SetAudienceResolver wires the SpiceDB-backed audience resolver onto this
// Kind. Called once at startup by channelsd/main.go after the SpiceDB client
// is ready; nil before that point. Nil-safe: AudienceCapability and
// ResolveAudience degrade gracefully when not wired.
func (k *Kind) SetAudienceResolver(sdb spiceDBLookup) {
	k.audience = &audienceResolver{sdb: sdb}
}

// AudienceCapability reports that the Slack kind can fully resolve the
// audience for a channel (CapabilityFull). Static; no I/O.
func (k *Kind) AudienceCapability() channelkinds.Capability {
	return channelkinds.CapabilityFull
}

// ResolveAudience delegates to the wired audienceResolver. Returns an
// error if SetAudienceResolver has not been called.
func (k *Kind) ResolveAudience(ctx context.Context, sess channelkinds.SessionInfo) ([]string, error) {
	if k.audience == nil {
		return nil, fmt.Errorf("slack audience resolver not wired")
	}
	return k.audience.ResolveAudience(ctx, sess)
}

// slackListener lives in listener.go; constructed via newSlackListener.
// slackSender lives in sender.go; constructed via newSlackSender.
// slackWizard lives in wizard.go, constructed by Kind.Wizard above; the
// monitoring half of the flow lives in wizard_monitoring.go.
