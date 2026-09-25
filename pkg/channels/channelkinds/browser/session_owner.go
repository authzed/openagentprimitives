package browser

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// SessionOwner makes the browser kind a channelkinds.SessionOwnerProvider: the
// idp-authenticated sender of each inbound chat message is the session's
// starting user, and so its owner. A browser Channel is user-attributable, so
// this satisfies the channel controller's owner-policy resolvability check
// WITHOUT an explicit spec.owner.
//
// The webd chat backend stamps the resolved canonical subject straight onto the
// AgentSession's AnnotationStartedByCanonicalID at creation, so it does not
// depend on the inbound external→canonical resolution this method drives. This
// exists for the controller's pre-flight probe and for parity with the other
// user-attributable kinds.
func (*Kind) SessionOwner(ev channelkinds.InboundEvent) (string, bool) {
	if ev.ExternalIDs.ExternalID == "" && ev.ExternalIDs.Email == "" {
		return "", false
	}
	// Guest without a verified email is keyed by the synthetic subject
	// (matching canonicalID), so opt in. With the guard above this is total;
	// fail safe (no owner) on the unreachable error rather than mint a phantom.
	subj, err := identity.FromExternal(identity.Kind(ev.ExternalIDs.Kind), identity.TeamScope(ev.ExternalIDs.TeamScope),
		identity.RawExternalID(ev.ExternalIDs.ExternalID), identity.Email(ev.ExternalIDs.Email)).AllowSynthetic().Subject()
	if err != nil {
		return "", false
	}
	// identity boundary: the SessionOwner resolver returns a (string, bool) subject; serialized here.
	return subj.String(), true
}
