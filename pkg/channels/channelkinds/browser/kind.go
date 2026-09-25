// Package browser is the channel kind for a session a BROWSER is looking at:
// webd serves the page, the page holds the socket, and this kind's
// Listener/Sender/StreamDeltaSink run in that host process rather than in
// channelsd (RelayedByChannelsd reports false). It near-clones `local`, the
// TUI kind, which has the same client-hosted shape with a terminal on the
// other end instead of a browser.
//
// The bare Kind.NewListener / NewSender return inert no-op objects for
// interface compliance (registry lookup, ValidateSpec, the channel
// controller's validation). The objects that actually carry traffic come from
// NewHost (host.go), which binds an EventSink the generic channelkinds.Deps
// cannot carry. pkg/web/webui/chat supplies a websocket-backed EventSink today
// (its wsSink); the local TUI kind wraps a bubbletea program in the same
// slot, which is what makes the abstraction transport-agnostic rather than
// browser-specific.
package browser

import (
	"context"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// KindName is the Channel.spec.kind discriminator for this kind. It is also
// the value stored in the AgentSession's channel-kind label and in
// spec.inputChannel.kind.
const KindName = "browser"

func init() { registry.Register(&Kind{}) }

// Kind is the registered browser channel kind. It is stateless — all
// per-session state lives in the Host (host.go) and the browser-side UI.
type Kind struct{}

func (*Kind) Name() string { return KindName }

// DefaultSessionScope: a browser session is always one user (the viewer
// looking at the page); "user" scope is the correct correlation granularity.
func (*Kind) DefaultSessionScope() string { return "user" }

// Capabilities: text/markdown/plan are inline-render capabilities;
// asset:text/html is the ATTACH capability, enabling respond_to_user's
// `attached` field. The page delivers such an attachment as a same-origin
// download chip, NOT by rendering the HTML inline — so this advertises
// attachment delivery, not inline rendering. Producing and live-viewing an
// artifact is a separate axis, ungated by this.
func (*Kind) Capabilities() []string {
	return []string{"text", "markdown", "plan", "asset:text/html"}
}

// NewListener returns an inert listener. Real inbound handling is wired
// by NewHost; the bare path exists only for interface compliance.
func (*Kind) NewListener(channelkinds.Deps) channelkinds.Listener {
	return noopListener{}
}

// NewSender returns an inert sender. See NewListener.
func (*Kind) NewSender(channelkinds.Deps) channelkinds.Sender {
	return noopSender{}
}

// SubChannelSender returns an inert sender for known sub-channels, nil
// for unknown ones. Real sub-channel senders come from the Host.
func (*Kind) SubChannelSender(name string, _ channelkinds.Deps) channelkinds.Sender {
	switch name {
	case "message", "tool_session", "permission_request":
		return noopSender{}
	default:
		return nil
	}
}

// NewStreamDeltaSink returns nil on the bare path — the Host supplies
// the real sink. Returning nil here is harmless: channelsd never routes
// to a browser kind (RelayedByChannelsd()==false).
func (*Kind) NewStreamDeltaSink(channelkinds.Deps) channelkinds.StreamDeltaSink {
	return nil
}

// SupportsMonitoring is false: browser has no MonitoringSender.
func (*Kind) SupportsMonitoring() bool { return false }

// SupportsLiveViewOffer is true: the Host's live_view_offer sender mints a webd
// deep-link and emits it as a timeline note. As with local, the bare
// SubChannelSender above returns nil for it because the real sender belongs to
// the Host, so the answer has to be declared rather than looked up.
func (*Kind) SupportsLiveViewOffer() bool { return true }

// NewMonitoringSender: browser is client-hosted and never delivers
// monitoring messages via channelsd.
func (*Kind) NewMonitoringSender(_ channelkinds.Deps) channelkinds.MonitoringSender { return nil }

// SupportedRoles is every role: a browser surface both submits the viewer's
// messages and renders the agent's replies. Stated as the full set rather than
// nil — nil means "serves no role", which is not what this kind means.
func (*Kind) SupportedRoles() []string { return spiceboxv1alpha1.AllChannelRoles() }

// ValidateSpec rejects channels that mix the browser kind with another
// kind's config block. The browser kind needs no kind-specific config.
func (*Kind) ValidateSpec(ch *spiceboxv1alpha1.Channel) error {
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
	return nil
}

// RequiredSecretKeys: the browser kind needs no Secret keys. The
// ephemeral Channel CR its host process creates still references a
// Secret name for CRD-shape reasons, but no data keys are required.
func (*Kind) RequiredSecretKeys(*spiceboxv1alpha1.Channel) []string { return nil }

// PublicSecretKeys is nil: this kind declares no Secret shape, so it can
// vouch for no key in one. Anything an operator put there stays secret.
func (*Kind) PublicSecretKeys(*spiceboxv1alpha1.Channel) []string { return nil }

// FeatureSupport reports no transport permissions: this kind has no
// permission model, so every feature it can express is free.
func (*Kind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return nil
}

// RenderMention: the browser kind has no @-tag syntax; return the bare id.
func (*Kind) RenderMention(externalID string) string { return externalID }

// SupportedMentionLookups: none. The runner omits
// lookup_user_for_mention from browser-bound sessions.
func (*Kind) SupportedMentionLookups() []channelkinds.MentionLookupKind { return nil }

// LookupUser always returns ErrMentionUnsupported — the tool is omitted
// from browser-bound sessions, so this is reached only via deliberate
// bypass.
func (*Kind) LookupUser(
	context.Context, channelkinds.LookupDeps,
	channelkinds.MentionLookupKind, string,
) (string, string, error) {
	return "", "", channelkinds.ErrMentionUnsupported
}

// MentionToolDescription: empty; no mention tool is registered.
func (*Kind) MentionToolDescription() string { return "" }

// TextFormattingInstructions implements channelkinds.TextFormatter. The web
// chat renders replies through the shared design-system <Markdown>, which is
// react-markdown + remark-gfm with rehype-raw deliberately absent — so GFM
// works and raw HTML is escaped rather than rendered.
func (*Kind) TextFormattingInstructions() string {
	return "Markdown (GitHub-flavored) is supported — headings, lists, tables, " +
		"links, and fenced code blocks all render. Raw HTML is escaped, not rendered, " +
		"so do not emit HTML tags."
}

// UserAttributable: a browser session is driven by exactly one viewer —
// the host process's authenticated user, who is the session's started_by.
// True.
func (*Kind) UserAttributable() bool { return true }

// DeliversToHuman is true: the surface is a browser tab a person is looking
// at.
func (*Kind) DeliversToHuman() bool { return true }

// AllowsSyntheticIdentity: false. webd is IdP-backed — its users always
// carry a verified email, so an email-less author claim on a
// browser-bound session is a forgery attempt and must fail closed.
func (*Kind) AllowsSyntheticIdentity() bool { return false }

// RelayedByChannelsd: false. The browser transport runs in whatever host
// process serves the page (webd today); it cannot exist in a channelsd pod.
func (*Kind) RelayedByChannelsd() bool { return false }

// SpawnsSessionOnInbound: false. This kind owns exactly ONE AgentSession for
// the lifetime of the browser session its host pre-created; an inbound with no
// active session must not spawn a phantom one.
func (*Kind) SpawnsSessionOnInbound() bool { return false }

// WebAuthenticator returns nil — the browser kind is driven by the host
// process's already-authenticated user; it has no web-based OIDC flow of
// its own.
func (*Kind) WebAuthenticator(channelkinds.WebAuthDeps) channelkinds.WebAuthenticator { return nil }

// WebhookReceiver returns nil: this kind has no inbound HTTP surface.
func (*Kind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver { return nil }

// Wizard refuses: the browser kind's host process builds the ephemeral
// Channel CR itself, so there is no setup flow to run. The interface still
// requires a non-nil implementation.
func (*Kind) Wizard() channelkinds.Wizard {
	return channelkinds.UnavailableWizard("browser kind has no interactive wizard; it is created by its host process")
}

// AudienceCapability returns CapabilitySingleUser for browser channels.
func (k *Kind) AudienceCapability() channelkinds.Capability {
	return audienceResolver.AudienceCapability()
}

// ResolveAudience delegates to the package's audienceResolver.
func (k *Kind) ResolveAudience(ctx context.Context, sess channelkinds.SessionInfo) ([]string, error) {
	return audienceResolver.ResolveAudience(ctx, sess)
}

// IsBrowserSurface implements channelkinds.BrowserSurface: a session bound to
// this kind is, by definition, already being read in a browser on webd's own
// trusted origin.
func (*Kind) IsBrowserSurface() bool { return true }

func errSpecForeignBlock(block string) error {
	return fmt.Errorf("spec.%s must be empty when kind=%q", block, KindName)
}

// noopListener / noopSender are the inert objects returned by the bare
// Kind methods. They never run in production — the real host always uses
// the Host's real objects. Start/Stop succeed silently; Send errors so a
// mis-wired caller fails loudly rather than dropping output.
type noopListener struct{}

func (noopListener) Start(context.Context) error { return nil }
func (noopListener) Stop(context.Context) error  { return nil }

type noopSender struct{}

func (noopSender) Send(
	context.Context, channelkinds.SessionInfo, channelevents.Envelope,
) (channelkinds.SubChannelSendResult, error) {
	return channelkinds.SubChannelSendResult{}, fmt.Errorf(
		"browser kind: bare Sender invoked; use browser.NewHost for the UI-bound sender")
}
