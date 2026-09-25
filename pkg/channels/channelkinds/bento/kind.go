// Package bento is the channel kind that schedules inbound messages
// from an embedded Bento (https://warpstreamlabs.github.io/bento)
// `generate` input. Cron-style triggers without a human user;
// pairs with a sibling output-role Channel for the response surface.
package bento

import (
	"context"
	"fmt"
	"slices"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// Kind implements channelkinds.Kind for the embedded Bento input.
type Kind struct{}

// Compile-time interface check.
var _ channelkinds.Kind = (*Kind)(nil)

// Name returns the registry name.
func (Kind) Name() string { return "bento" }

// DefaultSessionScope is `thread` so replies in the output Channel's
// thread route back to the same AgentSession (via the
// outputChannel.Key index extension).
func (Kind) DefaultSessionScope() string { return "thread" }

// Capabilities are intentionally empty — bento has no rendering
// surface of its own; it's input-only.
func (Kind) Capabilities() []string { return nil }

// NewListener constructs the bento stream-host listener. Stream
// lifecycle is implemented in listener.go.
func (Kind) NewListener(deps channelkinds.Deps) channelkinds.Listener {
	return newListener(deps)
}

// NewSender is unused — bento channels must be role=input. The
// channel controller's ValidateSpec rejects role=output|both for
// kind=bento; this returns a no-op sender as defense-in-depth.
func (Kind) NewSender(_ channelkinds.Deps) channelkinds.Sender { return nopSender{} }

// SubChannelSender returns nil; bento has no sub-channels.
func (Kind) SubChannelSender(_ string, _ channelkinds.Deps) channelkinds.Sender { return nil }

// NewStreamDeltaSink returns nil; bento has no rendering surface.
func (Kind) NewStreamDeltaSink(_ channelkinds.Deps) channelkinds.StreamDeltaSink { return nil }

// SupportsMonitoring is false: bento has no MonitoringSender.
func (Kind) SupportsMonitoring() bool { return false }

// SupportsLiveViewOffer is false: bento is input-only and has no outbound
// surface at all to render an offer on.
func (Kind) SupportsLiveViewOffer() bool { return false }

// NewMonitoringSender: bento is input-only and never delivers
// monitoring messages.
func (Kind) NewMonitoringSender(_ channelkinds.Deps) channelkinds.MonitoringSender { return nil }

// SupportedRoles is input only: this kind is a cron-style trigger with no
// rendering surface of its own — the response goes out on the sibling
// output-role Channel (see the package doc).
func (Kind) SupportedRoles() []string {
	return []string{spiceboxv1alpha1.ChannelRoleInput}
}

// ValidateSpec rejects every role this kind does not serve and requires
// spec.bento.generate.
func (k Kind) ValidateSpec(ch *spiceboxv1alpha1.Channel) error {
	if !slices.Contains(k.SupportedRoles(), ch.Spec.Role) {
		return fmt.Errorf("bento channels must declare role=%s; got %q",
			strings.Join(k.SupportedRoles(), "|"), ch.Spec.Role)
	}
	if ch.Spec.Bento == nil || ch.Spec.Bento.Generate == nil {
		return fmt.Errorf("bento channel %q: spec.bento.generate is required",
			ch.Name)
	}
	return nil
}

// RequiredSecretKeys returns nil; the bento generate input has no
// per-Channel secret of its own. The Channel still has a CredentialsRef
// (schema requirement); we tolerate an empty Secret.
func (Kind) RequiredSecretKeys(_ *spiceboxv1alpha1.Channel) []string { return nil }

// PublicSecretKeys is nil: this kind declares no Secret shape, so it can
// vouch for no key in one. Anything an operator put there stays secret.
func (Kind) PublicSecretKeys(_ *spiceboxv1alpha1.Channel) []string { return nil }

// FeatureSupport reports no transport permissions: this kind has no
// permission model, so every feature it can express is free.
func (Kind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return nil
}

// RenderMention returns the bare externalID; bento has no mention
// syntax of its own.
func (Kind) RenderMention(externalID string) string { return externalID }

// SupportedMentionLookups returns nil — bento sessions are cron-
// spawned with no human directory to look against.
func (Kind) SupportedMentionLookups() []channelkinds.MentionLookupKind { return nil }

// LookupUser returns ErrMentionUnsupported; never called given the
// empty SupportedMentionLookups.
func (Kind) LookupUser(_ context.Context, _ channelkinds.LookupDeps, _ channelkinds.MentionLookupKind, _ string) (string, string, error) {
	return "", "", channelkinds.ErrMentionUnsupported
}

// MentionToolDescription is unused for bento; runner gates on
// SupportedMentionLookups before reading it.
func (Kind) MentionToolDescription() string { return "" }

// TextFormattingInstructions implements channelkinds.TextFormatter. Bento is
// input-only (Capabilities() is nil, NewSender is a nop), so no reply is ever
// rendered on it and the answer says so rather than promising a dialect the
// transport does not have. It is still declared, not omitted: the registry
// sweep requires every kind to answer, and "there is no rendering surface" is
// an answer — silence would leave the caller guessing.
func (Kind) TextFormattingInstructions() string {
	return "This channel has no rendering surface — it is input-only, and replies " +
		"are never displayed on it. Send plain text."
}

// UserAttributable is false — bento has no end-user attribution.
// AgentClass validator requires Channel.spec.authzSubject when
// binding to bento input Channels.
func (Kind) UserAttributable() bool { return false }

// DeliversToHuman is false. A bento Channel is an input the scheduler writes
// into; TextFormattingInstructions above says the same thing from the other
// side — "this channel has no rendering surface … replies are never displayed
// on it". There is nobody watching it, so a prompt whose answer is a person's
// decision must be routed somewhere else.
func (Kind) DeliversToHuman() bool { return false }

// AllowsSyntheticIdentity: false. bento sessions attribute work via
// spec.authzSubject/RawSubject (a pre-formed, validated SpiceDB subject),
// never a per-user email-less identity, and bento has no view_message
// inbound path for this gate to apply to.
func (Kind) AllowsSyntheticIdentity() bool { return false }

// RelayedByChannelsd: bento's embedded stream runs inside channelsd. True.
func (Kind) RelayedByChannelsd() bool { return true }

// SpawnsSessionOnInbound: a bento Channel is a durable, scheduler-driven
// input that fires repeatedly — each tick that finds no active session
// legitimately spawns a fresh one. True.
func (Kind) SpawnsSessionOnInbound() bool { return true }

// WebAuthenticator returns nil — bento is scheduler-driven with no human
// user to authenticate via OIDC.
func (Kind) WebAuthenticator(channelkinds.WebAuthDeps) channelkinds.WebAuthenticator { return nil }

// WebhookReceiver returns nil: this kind has no inbound HTTP surface.
func (Kind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver { return nil }

// Wizard returns the interactive `oap channel create --kind bento`
// flow. Implemented in wizard.go.
func (Kind) Wizard() channelkinds.Wizard { return &wizard{} }

type nopSender struct{}

func (nopSender) Send(_ context.Context, _ channelkinds.SessionInfo, _ channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	return channelkinds.SubChannelSendResult{}, fmt.Errorf("bento channels are input-only; no Send")
}
