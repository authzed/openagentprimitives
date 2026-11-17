package fake_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
)

var _ channelkinds.GroupGoalHost = fake.Kind{}

func TestGroupGoalHost(t *testing.T) {
	k := fake.Kind{}
	b := &spiceboxv1alpha1.ChannelBinding{Kind: "fake", Key: "k1", External: map[string]string{"group": "g1"}}

	m, ok := k.ConversationMemberRef(b)
	assert.True(t, ok)
	assert.Equal(t, "group:g1#member", m)
	a, ok := k.ConversationAudienceRef(b)
	assert.True(t, ok)
	assert.Equal(t, "group:g1#member", a)

	_, ok = k.ConversationMemberRef(&spiceboxv1alpha1.ChannelBinding{Kind: "fake"})
	assert.False(t, ok, "no group fixture key: not a group conversation")
	_, ok = k.ConversationAudienceRef(nil)
	assert.False(t, ok)

	assert.Equal(t, []string{"group#member"}, k.GroupMemberLinks())
	require.NoError(t, k.MembershipCurrent(context.Background(), nil, &spiceboxv1alpha1.Channel{}))

	orig := b.DeepCopy()
	r1, err := k.RunAnchor(b)
	require.NoError(t, err)
	r2, err := k.RunAnchor(b)
	require.NoError(t, err)
	assert.Equal(t, orig, b, "input must not be mutated")
	assert.NotEqual(t, b.Key, r1.Key)
	assert.NotEqual(t, r1.Key, r2.Key, "each run anchor is a fresh fixture key")
	assert.Equal(t, "g1", r1.External["group"], "same conversation")
}
