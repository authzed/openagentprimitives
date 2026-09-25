package browser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// A browser Channel must pass the channel controller's owner-policy
// resolvability check (validate() in pkg/controllers/channel/controller.go)
// WITHOUT an explicit/ownerless spec.owner — the way a user-attributable Slack
// channel does. That requires the kind to implement SessionOwnerProvider and to
// report "provides a starter" for the controller's dummy probe event; before
// this the built-in web chat's ephemeral Channel went Valid=False/SpecInvalid
// ("ownerless input requires spec.owner.explicit or spec.owner.ownerless").
func TestSessionOwner(t *testing.T) {
	var k any = &Kind{}
	so, ok := k.(channelkinds.SessionOwnerProvider)
	require.True(t, ok, "browser must implement channelkinds.SessionOwnerProvider")

	subj, provides := so.SessionOwner(channelkinds.InboundEvent{
		ExternalIDs: channelkinds.ExternalIdentity{Kind: KindName, ExternalID: "probe"},
	})
	assert.True(t, provides, "an inbound carrying a sender external id must yield a starting user")
	assert.NotEmpty(t, subj, "a provided starter must be a non-empty subject")

	_, provides = so.SessionOwner(channelkinds.InboundEvent{
		ExternalIDs: channelkinds.ExternalIdentity{Kind: KindName},
	})
	assert.False(t, provides, "no external id and no email → no starter (fail safe)")
}
