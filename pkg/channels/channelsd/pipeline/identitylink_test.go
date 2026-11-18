package pipeline

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestUpsertChannelIdentity(t *testing.T) {
	slackAlice := spiceboxv1alpha1.ChannelIdentity{Kind: "slack", Domain: "T0COMPANY", ExternalID: "U0ALICE", DisplayName: "Alice"}

	// New entry: appended, changed=true.
	got, changed := upsertChannelIdentity(nil, slackAlice)
	assert.True(t, changed)
	assert.Equal(t, []spiceboxv1alpha1.ChannelIdentity{slackAlice}, got)

	// Same tuple, same display: no-op, changed=false.
	got2, changed2 := upsertChannelIdentity(got, slackAlice)
	assert.False(t, changed2)
	assert.Len(t, got2, 1)

	// Same tuple, new display: in-place update, changed=true.
	renamed := slackAlice
	renamed.DisplayName = "Alice A."
	got3, changed3 := upsertChannelIdentity(got, renamed)
	assert.True(t, changed3)
	assert.Equal(t, "Alice A.", got3[0].DisplayName)

	// Same tuple, real name learned: also an update, so an identity recorded
	// before real names were captured picks one up on the user's next message.
	withReal := renamed
	withReal.RealName = "Alice Anders"
	gotReal, changedReal := upsertChannelIdentity(got3, withReal)
	assert.True(t, changedReal)
	assert.Equal(t, "Alice Anders", gotReal[0].RealName)

	// Same person, DIFFERENT workspace: a second entry (one canonical -> many).
	other := spiceboxv1alpha1.ChannelIdentity{Kind: "slack", Domain: "T0PARTNER", ExternalID: "U0GUEST", DisplayName: "Alice"}
	got4, changed4 := upsertChannelIdentity(got3, other)
	assert.True(t, changed4)
	assert.Len(t, got4, 2)
}
