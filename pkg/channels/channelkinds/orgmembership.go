package channelkinds

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// OrgMembershipAttributor is an optional Kind capability: a kind that
// classifies inbound users' standing in the org/workspace the channel is
// installed into stamps ExternalIdentity.OrgMembership and answers true here
// for that channel. The channelsd pipeline gates NEW-session creation on it —
// a non-member (or an identity the kind failed to stamp) must hold
// agentclass#start_session or be parked for admin approval.
//
// The Channel is an input because attribution can be per-channel config, not
// only per-kind: slack always attributes (every user resolves through
// users.info); the fake kind attributes only when the Channel's fake config
// opts in, so ordinary test channels keep their pre-gate behavior.
//
// Not implementing it means "this kind has no org to be a member of" — the
// gate does not apply and session start behaves as it always has. A kind
// that DOES attribute for a channel owns the fail-closed half of the
// contract: every identity it resolves must carry member or guest, because
// the pipeline reads an empty stamp from an attributing channel as guest.
type OrgMembershipAttributor interface {
	AttributesOrgMembership(ch *spiceboxv1alpha1.Channel) bool
}
