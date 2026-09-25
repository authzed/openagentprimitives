package fake

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// SessionOwner returns the canonical SpiceDB user subject for the inbound
// user, or ("", false) when the event carries no external identity.
//
// fake stands in for a user-attributed transport in tests, so it derives the
// owner exactly as slack does: the inbound user IS the session owner, and a
// user with no verified email is keyed by the synthetic subject, matching what
// the pipeline's own canonicalization does for the same event.
//
// Declaring this is not cosmetic. The channel controller probes for
// SessionOwnerProvider to decide whether a Channel needs an explicit owner
// policy; without it every fake Channel is judged "ownerless input" and held
// Valid=False — which production refuses to serve traffic on, while the test
// harness starts listeners without consulting validity and serves it anyway.
func (Kind) SessionOwner(ev channelkinds.InboundEvent) (string, bool) {
	if ev.ExternalIDs.ExternalID == "" && ev.ExternalIDs.Email == "" {
		return "", false
	}
	subj, err := identity.FromExternal(
		identity.Kind(ev.ExternalIDs.Kind),
		identity.TeamScope(ev.ExternalIDs.TeamScope),
		identity.RawExternalID(ev.ExternalIDs.ExternalID),
		identity.Email(ev.ExternalIDs.Email),
	).AllowSynthetic().Subject()
	if err != nil {
		// Fail safe: no owner rather than a phantom one. Unreachable given the
		// guard above plus AllowSynthetic, which together make this total.
		return "", false
	}
	return subj.String(), true
}

// Compile-time check: the capability the channel controller probes for is
// actually declared. Implementing the method without this assertion would let
// a later refactor drop it back off the interface silently.
var _ channelkinds.SessionOwnerProvider = Kind{}
