package local

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// A local Channel must pass the channel controller's owner-policy resolvability
// check (validate() in pkg/controllers/channel/controller.go) WITHOUT an
// explicit/ownerless spec.owner — the way a user-attributable Slack channel
// does. `oap agent chat`'s ephemeral Channel (cmd/oap's buildChatChannel) sets no
// spec.owner, so without SessionOwnerProvider the controller's dummy probe sees
// no starter and the Channel goes Valid=False/SpecInvalid ("ownerless input
// requires spec.owner.explicit or spec.owner.ownerless"). That is the same
// failure commit 486febfe fixed for the built-in web chat.
func TestSessionOwner(t *testing.T) {
	var k any = &Kind{}
	so, ok := k.(channelkinds.SessionOwnerProvider)
	require.True(t, ok, "local must implement channelkinds.SessionOwnerProvider")

	subj, provides := so.SessionOwner(channelkinds.InboundEvent{
		ExternalIDs: channelkinds.ExternalIdentity{Kind: KindName, ExternalID: "probe"},
	})
	assert.True(t, provides, "an inbound carrying a sender external id must yield a starting user")
	assert.NotEmpty(t, subj, "a provided starter must be a non-empty subject")

	// The TUI user is legitimately email-less on a no-IdP cluster
	// (AllowsSyntheticIdentity is true here, unlike browser), so an external id
	// alone MUST be enough — keying off the synthetic subject is the whole point.
	_, provides = so.SessionOwner(channelkinds.InboundEvent{
		ExternalIDs: channelkinds.ExternalIdentity{Kind: KindName},
	})
	assert.False(t, provides, "no external id and no email → no starter (fail safe)")
}
