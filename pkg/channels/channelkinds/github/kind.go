// Package github is the channel kind that receives GitHub pull-request
// webhook deliveries. Input-only and non-attributable: the PR author has no
// AP identity and consented to nothing, so every inbound event is attributed
// via the Channel's own authzSubject rather than a per-user identity.
// DefaultSessionScope is "thread" — one session per PR, so every event on
// the same pull request lands in the same AgentSession and thread.
//
// Unlike slack/bento, this kind's inbound surface is NOT a channelsd-hosted
// Listener: deliveries arrive over HTTP at webd (see
// pkg/web/webui/channelwebhook, mounted via a registry sweep over
// Kind.WebhookReceiver), and RelayedByChannelsd is false accordingly. The
// bare NewListener/NewSender below exist only for interface compliance, the
// same shape as browser/local's inert no-ops — the meaningful inbound path is
// WebhookReceiver, implemented in receiver.go. Replies are never rendered on
// this kind: Capabilities is nil, and NewSender's Send always errors — a
// reply goes out on the paired output Channel (a Slack channel, today)
// instead.
package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// Kind implements channelkinds.Kind for inbound GitHub pull-request webhook
// deliveries.
type Kind struct{}

// Compile-time interface check.
var _ channelkinds.Kind = (*Kind)(nil)

// Name returns the registry name.
func (Kind) Name() string { return "github" }

// DefaultSessionScope is "thread": every webhook event on the same pull
// request correlates to the same AgentSession via the ChannelKey the
// WebhookReceiver derives ("pr:<owner>/<repo>#<number>").
func (Kind) DefaultSessionScope() string { return "thread" }

// Capabilities are intentionally empty — github has no rendering surface of
// its own; it's input-only. Replies are posted on the paired output Channel.
func (Kind) Capabilities() []string { return nil }

// NewListener returns an inert listener. github has no channelsd-hosted
// transport to run — deliveries arrive at webd via WebhookReceiver — so this
// exists only for interface compliance.
func (Kind) NewListener(_ channelkinds.Deps) channelkinds.Listener { return nopListener{} }

// NewSender returns a sender that always refuses. github channels must be
// role=input; the channel controller's ValidateSpec rejects role=output|both
// for kind=github. Unlike a nil return, this still satisfies the Sender
// interface so a caller that forgets to check for input-only kinds fails with
// a clear error instead of a nil-pointer panic.
func (Kind) NewSender(_ channelkinds.Deps) channelkinds.Sender { return nopSender{} }

// SubChannelSender returns nil; github has no sub-channels.
func (Kind) SubChannelSender(_ string, _ channelkinds.Deps) channelkinds.Sender { return nil }

// NewStreamDeltaSink returns nil; github has no rendering surface.
func (Kind) NewStreamDeltaSink(_ channelkinds.Deps) channelkinds.StreamDeltaSink { return nil }

// SupportsMonitoring is false: github has no MonitoringSender.
func (Kind) SupportsMonitoring() bool { return false }

// SupportsLiveViewOffer is false: github is input-only — deliveries arrive at
// webd as webhooks and nothing is posted back — so there is no surface to
// render an offer on.
func (Kind) SupportsLiveViewOffer() bool { return false }

// NewMonitoringSender: github is input-only and never delivers monitoring
// messages.
func (Kind) NewMonitoringSender(_ channelkinds.Deps) channelkinds.MonitoringSender { return nil }

// SupportedRoles is input only: deliveries arrive by webhook and every reply
// goes out on the paired output Channel (see the package doc — Capabilities is
// nil and NewSender always errors), so there is nothing an output-, both-, or
// monitoring-role github Channel could do.
func (Kind) SupportedRoles() []string {
	return []string{spiceboxv1alpha1.ChannelRoleInput}
}

// ValidateSpec requires a role this kind serves and spec.github, and rejects a
// sibling spec.slack block (the "exactly one kind block matches kind"
// invariant every other kind enforces for its own block).
func (k Kind) ValidateSpec(ch *spiceboxv1alpha1.Channel) error {
	if !slices.Contains(k.SupportedRoles(), ch.Spec.Role) {
		return fmt.Errorf("github channels must declare role=%s; got %q",
			strings.Join(k.SupportedRoles(), "|"), ch.Spec.Role)
	}
	if ch.Spec.GitHub == nil {
		return fmt.Errorf("github channel %q: spec.github is required", ch.Name)
	}
	if ch.Spec.Slack != nil {
		return fmt.Errorf("github channel %q: must not set spec.slack", ch.Name)
	}
	return nil
}

// RequiredSecretKeys are the four keys the channel needs from its
// CredentialsRef Secret: the three the githubApp credkind also reads to mint
// installation tokens (app-id, private-key, installation-id), plus
// webhook-secret — which the credkind deliberately excludes, since minting a
// token and verifying a delivery signature are different operations against
// the same Secret. See pkg/platform/identity/credkind/githubapp's
// RequiredSecretKeys doc for the credkind side of this asymmetry.
func (Kind) RequiredSecretKeys(_ *spiceboxv1alpha1.Channel) []string {
	return []string{"app-id", "private-key", "webhook-secret", "installation-id"}
}

// PublicSecretKeys is app-id and installation-id — the same two the githubApp
// credkind declares, because it is the same Secret: the wizard here writes all
// four keys and that credential type reads three of them.
//
// Both are identifiers GitHub publishes. app-id is in the URL of the App's own
// settings page; installation-id arrives in the body of every webhook delivery
// GitHub sends, which is why it cannot be kept out of a verbatim record of what
// was delivered. private-key and webhook-secret are absent: those two ARE the
// credential, and a consumer told otherwise would write one down.
func (Kind) PublicSecretKeys(_ *spiceboxv1alpha1.Channel) []string {
	return []string{"app-id", "installation-id"}
}

// FeatureSupport declares scopes for exactly one feature,
// channelfeatures.DirectorySync — every other feature this kind might one
// day support would carry no Scopes here, since the App's own installation
// permissions are configured on the App, not negotiated per feature at
// runtime. DirectorySync is the exception because it is answered by neither:
// relsync_kind.go's SyncKind enumerates an org's teams/repos/members against
// a personal-access-token-style credential (relsync.SourceParams.Token),
// resolved independently of this Channel's own AgentIdentity, and REST calls
// made with THAT token carry their own OAuth scopes.
// TestDirectorySyncFeatureDeclaresTheScopesItsCallsNeed
// (relsync_scopes_test.go) drives ListScopes/FetchScope for real and pins
// this declaration against every endpoint they call — this repo shipped
// exactly this gap once, for Slack.
func (Kind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return map[channelfeatures.Feature]channelkinds.FeatureRequirement{
		channelfeatures.DirectorySync: {
			Scopes: []string{"read:org", "repo"},
			Setup: "Grant the token read:org and repo scope (a fine-grained token needs " +
				"organization members: read and repository metadata: read).",
			Degrades: "a RelationshipSource using this token cannot enumerate the organization at all: " +
				"the members, teams, repos and collaborators endpoints all return 403, so the periodic " +
				"GitHub directory sync writes nothing to SpiceDB — see pkg/platform/relsync",
		},
	}
}

// RenderMention returns the bare externalID; github has no mention syntax
// this kind renders (a review comment's @-mentions are the agent's own
// output, composed by the tool that posts it, not by this kind).
func (Kind) RenderMention(externalID string) string { return externalID }

// SupportedMentionLookups returns nil — a pull request's participants have no
// AP directory to look up against.
func (Kind) SupportedMentionLookups() []channelkinds.MentionLookupKind { return nil }

// LookupUser returns ErrMentionUnsupported; never called given the empty
// SupportedMentionLookups.
func (Kind) LookupUser(context.Context, channelkinds.LookupDeps, channelkinds.MentionLookupKind, string) (string, string, error) {
	return "", "", channelkinds.ErrMentionUnsupported
}

// MentionToolDescription is unused for github; the runner gates on
// SupportedMentionLookups before reading it.
func (Kind) MentionToolDescription() string { return "" }

// TextFormattingInstructions implements channelkinds.TextFormatter. github is
// input-only (Capabilities() is nil, NewSender refuses), so no reply is ever
// rendered on it. Declared rather than omitted — the registry sweep requires
// every kind to answer, and "there is no rendering surface" is an answer.
func (Kind) TextFormattingInstructions() string {
	return "This channel has no rendering surface — it receives GitHub pull-request events only. " +
		"Replies are delivered on the paired output Channel."
}

// UserAttributable is false — the PR author has no AP identity and consented
// to nothing. The Channel's spec.authzSubject (or the bound AgentClass's
// interactPermission) attributes the resulting session instead.
func (Kind) UserAttributable() bool { return false }

// DeliversToHuman is false, for the same reason bento's is: this kind has no
// rendering surface at all. Capabilities is nil and NewSender always refuses,
// so a human-directed envelope routed to a github Channel would be dropped by
// the sender rather than read by the PR author. The relay's lineage walk
// climbs past it to the paired output Channel — the Slack channel a reviewer
// is actually watching — which is where a permission or credential decision
// has to be asked.
//
// Not the same fact as UserAttributable above, even though both are false
// here: that one is about inbound identity and gates the AgentClass binding
// validator. See the interface doc for why they stay separate.
func (Kind) DeliversToHuman() bool { return false }

// AllowsSyntheticIdentity is false. github sessions attribute work via the
// Channel's own authzSubject, never a per-user identity, so there is no
// view_message inbound path for a synthetic-identity gate to apply to.
func (Kind) AllowsSyntheticIdentity() bool { return false }

// RelayedByChannelsd is false: channelsd hosts no transport for this kind.
// Deliveries arrive over HTTP at webd (channelwebhook), which sweeps the
// registry for Kind.WebhookReceiver — see that method's doc and
// clientHostedHere's doc comment in internal/cmd/channelsd for how channelsd
// itself treats this.
func (Kind) RelayedByChannelsd() bool { return false }

// SpawnsSessionOnInbound is true: a never-reviewed pull request legitimately
// starts a session the first time its webhook fires, the same way a bento
// tick or a fresh Slack thread does.
func (Kind) SpawnsSessionOnInbound() bool { return true }

// WebAuthenticator returns nil — github has no human sign-in flow; a GitHub
// App installation is provisioned once by an operator, not per end user.
func (Kind) WebAuthenticator(channelkinds.WebAuthDeps) channelkinds.WebAuthenticator { return nil }

// WebhookReceiver returns the kind's inbound-HTTP receiver. github IS
// webhook-routable — this is its real inbound surface. See receiver.go.
func (Kind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver {
	return receiver{}
}

// Wizard returns the github setup flow (wizard_data.go): pick an AgentClass,
// name the Channel, then either an automated GitHub App-manifest browser
// round trip or a manual fallback, followed by installing the App and
// pasting back the resulting installation ID. Kind.Wizard returns a fresh
// value per call, so the convert collaborator is left nil here and defaulted
// lazily inside Handoff — the one method that needs it — the same pattern
// bento's Wizard uses.
func (Kind) Wizard() channelkinds.Wizard {
	return &wizard{}
}

// nopListener is the inert Listener returned by NewListener. github has no
// channelsd-hosted transport to start or stop.
type nopListener struct{}

func (nopListener) Start(context.Context) error { return nil }
func (nopListener) Stop(context.Context) error  { return nil }

// nopSender is the Sender returned by NewSender. Its Send always errors: a
// github channel is input-only, so nothing should ever call it, and a caller
// that does gets a clear error instead of a silent no-op or a nil-pointer
// panic.
type nopSender struct{}

var errGitHubChannelsAreInputOnly = errors.New("github channels are input-only; replies go to the paired output Channel")

func (nopSender) Send(context.Context, channelkinds.SessionInfo, channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	return channelkinds.SubChannelSendResult{}, errGitHubChannelsAreInputOnly
}
