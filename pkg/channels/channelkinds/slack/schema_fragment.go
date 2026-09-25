package slack

import (
	_ "embed"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// slackSchemaRawZed holds the Slack hierarchy; see schema.zed for what it
// declares and why.
//
//go:embed schema.zed
var slackSchemaRawZed string

// SpiceDBSchemaFragment returns the Slack hierarchy required by the
// info-leakage gate's audience-resolver lookups. Static; no I/O.
func (k *Kind) SpiceDBSchemaFragment() *spiceboxv1alpha1.SpiceDBSchemaFragment {
	return &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: slackSchemaRawZed,
	}
}

// SessionOwner returns the canonical SpiceDB user subject for the inbound Slack
// user. Returns ("", false) when no external identity is available.
func (k *Kind) SessionOwner(ev channelkinds.InboundEvent) (string, bool) {
	if ev.ExternalIDs.ExternalID == "" && ev.ExternalIDs.Email == "" {
		return "", false
	}
	// The inbound user becomes the session owner; a guest without a verified
	// email is keyed by the synthetic subject (matching canonicalID), so opt
	// in. With the opt-in + the guard above this is total; fail safe (no
	// owner) on the unreachable error rather than mint a phantom.
	subj, err := identity.FromExternal(identity.Kind(ev.ExternalIDs.Kind), identity.TeamScope(ev.ExternalIDs.TeamScope),
		identity.RawExternalID(ev.ExternalIDs.ExternalID), identity.Email(ev.ExternalIDs.Email)).AllowSynthetic().Subject()
	if err != nil {
		return "", false
	}
	// identity boundary: the resolver returns a (string, bool) subject; serialized here.
	return subj.String(), true
}

// OwnerGroupRef links owner to the Slack channel's synced membership group via
// the output channel's configured channel ID. Returns ("", false) when the
// channel or its OutputDefaults.ChannelID is absent.
func (k *Kind) OwnerGroupRef(ch *spiceboxv1alpha1.Channel) (string, bool) {
	if ch == nil || ch.Spec.Slack == nil || ch.Spec.Slack.OutputDefaults == nil ||
		ch.Spec.Slack.OutputDefaults.ChannelID == "" {
		return "", false
	}
	return "slack_channel:" + ch.Spec.Slack.OutputDefaults.ChannelID + "#member", true
}

// SessionRelationLinks returns the Slack group link-types admissible as
// owner/participant on an agentsession.
func (k *Kind) SessionRelationLinks() []string {
	return []string{"slack_channel#member", "slack_usergroup#member"}
}

// Compile-time checks: *Kind satisfies all optional capability interfaces.
var (
	_ channelkinds.SchemaContributor     = (*Kind)(nil)
	_ channelkinds.SessionOwnerProvider  = (*Kind)(nil)
	_ channelkinds.OwnerGroupProvider    = (*Kind)(nil)
	_ channelkinds.SessionRelationLinker = (*Kind)(nil)
)
