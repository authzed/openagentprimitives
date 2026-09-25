// Package local is the local-TUI channel kind. Its
// Listener/Sender/StreamDeltaSink are NOT hosted by channelsd
// (RelayedByChannelsd is false) — they run inside the `oap` process and render
// to a bubbletea terminal UI. The bare Kind.NewListener / NewSender return
// inert no-ops for interface compliance; the meaningful host-side objects come
// from NewHost, which binds the bubbletea program handle the generic
// channelkinds.Deps cannot carry.
package local

import (
	"context"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// KindName is the Channel.spec.kind discriminator for this kind.
const KindName = "local"

func init() { registry.Register(&Kind{}) }

// Kind is the registered local-TUI channel kind. It is stateless — all
// per-session state lives in the Host (host.go) and the bubbletea model.
type Kind struct{}

func (*Kind) Name() string { return KindName }

// DefaultSessionScope: a local TUI session is always one user (the local
// `oap` user); "user" scope is the correct correlation granularity.
func (*Kind) DefaultSessionScope() string { return "user" }

// Capabilities: the TUI renders text, markdown, and structured plans.
// No asset MIMEs — the terminal cannot display images; artifacts stay
// queryable via `oap artifact`.
func (*Kind) Capabilities() []string {
	return []string{"text", "markdown", "plan"}
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
// to a local kind (RelayedByChannelsd()==false).
func (*Kind) NewStreamDeltaSink(channelkinds.Deps) channelkinds.StreamDeltaSink {
	return nil
}

// SupportsMonitoring is false: local has no MonitoringSender.
func (*Kind) SupportsMonitoring() bool { return false }

// SupportsLiveViewOffer is true: the Host's live_view_offer sender mints a webd
// deep-link and renders it as a "View in browser" timeline note. The bare
// SubChannelSender above cannot show that — the real sender comes from the
// Host, in the user's own process — which is why the answer is declared here.
func (*Kind) SupportsLiveViewOffer() bool { return true }

// NewMonitoringSender: local is client-hosted and never delivers
// monitoring messages via channelsd.
func (*Kind) NewMonitoringSender(_ channelkinds.Deps) channelkinds.MonitoringSender { return nil }

// SupportedRoles is every role: the TUI both reads the operator's input and
// renders the reply, and `oap agent chat` creates its Channel as role=both.
// Stated as the full set rather than nil — nil means "serves no role", which
// is not what this kind means.
func (*Kind) SupportedRoles() []string { return spiceboxv1alpha1.AllChannelRoles() }

// ValidateSpec rejects channels that mix the local kind with another
// kind's config block. The local kind needs no kind-specific config.
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

// RequiredSecretKeys: the local kind needs no Secret keys. The
// ephemeral Channel CR `oap agent chat` creates still references a
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

// RenderMention: the local kind has no @-tag syntax; return the bare id.
func (*Kind) RenderMention(externalID string) string { return externalID }

// SupportedMentionLookups: none. The runner omits
// lookup_user_for_mention from local-bound sessions.
func (*Kind) SupportedMentionLookups() []channelkinds.MentionLookupKind { return nil }

// LookupUser always returns ErrMentionUnsupported — the tool is omitted
// from local-bound sessions, so this is reached only via deliberate
// bypass.
func (*Kind) LookupUser(
	context.Context, channelkinds.LookupDeps,
	channelkinds.MentionLookupKind, string,
) (string, string, error) {
	return "", "", channelkinds.ErrMentionUnsupported
}

// MentionToolDescription: empty; no mention tool is registered.
func (*Kind) MentionToolDescription() string { return "" }

// TextFormattingInstructions implements channelkinds.TextFormatter. The TUI
// timeline is a terminal pane, so the dialect is CommonMark but the shape
// matters: long lines wrap badly in a narrow pane, and there is no image or
// HTML surface to fall back on.
func (*Kind) TextFormattingInstructions() string {
	return "Markdown (CommonMark) is supported. This surface is a terminal pane: " +
		"prefer short lines and simple lists, and do not rely on images or HTML."
}

// UserAttributable: the local TUI is driven by exactly one user — the
// local `oap` operator, who is the session's started_by. True.
func (*Kind) UserAttributable() bool { return true }

// DeliversToHuman is true: the surface is a terminal pane the local `oap`
// operator is sitting in front of.
func (*Kind) DeliversToHuman() bool { return true }

// AllowsSyntheticIdentity: true. The local TUI's OS-user identity is
// legitimately synthetic on a no-IdP cluster — `oap`'s login mints it via
// identity.FromExternal("local", "", username, "").AllowSynthetic(), there
// being no verified email to carry. A view_message inbound on a local-bound
// session must resolve that same email-less identity, not fail closed on it.
func (*Kind) AllowsSyntheticIdentity() bool { return true }

// RelayedByChannelsd: false. The local transport is the user's
// terminal; it cannot exist in a channelsd pod.
func (*Kind) RelayedByChannelsd() bool { return false }

// SpawnsSessionOnInbound: false. This kind owns exactly ONE AgentSession for
// the lifetime of `oap agent chat`, which pre-creates it; an inbound with no
// active session must not spawn a phantom one.
func (*Kind) SpawnsSessionOnInbound() bool { return false }

// WebAuthenticator returns nil — the local TUI kind is driven by the
// authenticated `oap` user; it has no web-based OIDC flow.
func (*Kind) WebAuthenticator(channelkinds.WebAuthDeps) channelkinds.WebAuthenticator { return nil }

// WebhookReceiver returns nil: this kind has no inbound HTTP surface.
func (*Kind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver { return nil }

// Wizard refuses: `oap agent chat` builds the ephemeral Channel CR itself, so
// there is no setup flow to run. The interface still requires a non-nil
// implementation.
func (*Kind) Wizard() channelkinds.Wizard {
	return channelkinds.UnavailableWizard("local kind has no interactive wizard; use `oap agent chat`")
}

// AudienceCapability returns CapabilitySingleUser for local channels.
func (k *Kind) AudienceCapability() channelkinds.Capability {
	return audienceResolver.AudienceCapability()
}

// ResolveAudience delegates to the package's audienceResolver.
func (k *Kind) ResolveAudience(ctx context.Context, sess channelkinds.SessionInfo) ([]string, error) {
	return audienceResolver.ResolveAudience(ctx, sess)
}

func errSpecForeignBlock(block string) error {
	return fmt.Errorf("spec.%s must be empty when kind=%q", block, KindName)
}

// noopListener / noopSender are the inert objects returned by the bare
// Kind methods. They never run in production — `oap` always uses the
// Host's real objects. Start/Stop succeed silently; Send errors so a
// mis-wired caller fails loudly rather than dropping output.
type noopListener struct{}

func (noopListener) Start(context.Context) error { return nil }
func (noopListener) Stop(context.Context) error  { return nil }

type noopSender struct{}

func (noopSender) Send(
	context.Context, channelkinds.SessionInfo, channelevents.Envelope,
) (channelkinds.SubChannelSendResult, error) {
	return channelkinds.SubChannelSendResult{}, fmt.Errorf(
		"local kind: bare Sender invoked; use local.NewHost for the TUI-bound sender")
}
