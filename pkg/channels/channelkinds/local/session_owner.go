package local

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// SessionOwner makes the local kind a channelkinds.SessionOwnerProvider: the
// sender of each inbound TUI message is the session's starting user, and so its
// owner. A local Channel is user-attributable, so this satisfies the channel
// controller's owner-policy resolvability check WITHOUT an explicit spec.owner
// — which `oap agent chat`'s ephemeral Channel does not set, and whose absence
// would otherwise leave it Valid=False/SpecInvalid.
//
// `oap agent chat` writes the SpiceDB started_by relationship itself right after
// creating the session, so it does not depend on the inbound
// external→canonical resolution this method drives. This exists for the
// controller's pre-flight probe and for parity with the other
// user-attributable kinds.
func (*Kind) SessionOwner(ev channelkinds.InboundEvent) (string, bool) {
	if ev.ExternalIDs.ExternalID == "" && ev.ExternalIDs.Email == "" {
		return "", false
	}
	// A local `oap` user on a no-IdP cluster carries no verified email and is
	// keyed by the synthetic subject — the same AllowSynthetic() opt-in `oap`'s
	// login and the pipeline both make, which is why AllowsSyntheticIdentity is
	// true here. With the guard above this is total; fail safe (no owner) on
	// the unreachable error rather than mint a phantom.
	subj, err := identity.FromExternal(identity.Kind(ev.ExternalIDs.Kind), identity.TeamScope(ev.ExternalIDs.TeamScope),
		identity.RawExternalID(ev.ExternalIDs.ExternalID), identity.Email(ev.ExternalIDs.Email)).AllowSynthetic().Subject()
	if err != nil {
		return "", false
	}
	// identity boundary: the SessionOwner resolver returns a (string, bool) subject; serialized here.
	return subj.String(), true
}
