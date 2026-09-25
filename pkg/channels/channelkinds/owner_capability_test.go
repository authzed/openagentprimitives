package channelkinds_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

func TestSlackProvidesStarterAndGroup(t *testing.T) {
	k, ok := registry.Get("slack")
	require.True(t, ok, "slack kind must be registered")
	require.NotNil(t, k)

	so, ok := k.(channelkinds.SessionOwnerProvider)
	require.True(t, ok, "slack must implement SessionOwnerProvider")
	subj, provides := so.SessionOwner(channelkinds.InboundEvent{
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
	})
	assert.True(t, provides)
	assert.NotEmpty(t, subj)

	og, ok := k.(channelkinds.OwnerGroupProvider)
	require.True(t, ok, "slack must implement OwnerGroupProvider")
	// SlackChannelConfig has no top-level ChannelID; it lives on OutputDefaults.
	ref, ok2 := og.OwnerGroupRef(&spiceboxv1alpha1.Channel{Spec: spiceboxv1alpha1.ChannelSpec{
		Slack: &spiceboxv1alpha1.SlackChannelConfig{
			OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C9"},
		},
	}})
	assert.True(t, ok2)
	assert.Equal(t, "slack_channel:C9#member", ref)

	rl, ok := k.(channelkinds.SessionRelationLinker)
	require.True(t, ok, "slack must implement SessionRelationLinker")
	assert.ElementsMatch(t, []string{"slack_channel#member", "slack_usergroup#member"}, rl.SessionRelationLinks())
}

func TestBentoProvidesNeither(t *testing.T) {
	k, ok := registry.Get("bento")
	require.True(t, ok, "bento kind must be registered")
	require.NotNil(t, k)
	if so, ok := k.(channelkinds.SessionOwnerProvider); ok {
		_, provides := so.SessionOwner(channelkinds.InboundEvent{AuthzSubject: "service:digest"})
		assert.False(t, provides, "bento provides no starting owner")
	}
}
