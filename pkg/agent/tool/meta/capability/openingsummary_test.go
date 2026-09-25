package capability

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// TestOpeningSummaryCapability_OfferedForTriggerInputOnly is the gate stated
// directly, per the user's hard constraint: a human who starts an agent by
// typing in Slack must never see update_opening_summary — only a triggered,
// userless input (github today) that posts an opening line of its own gets
// the tool to enrich.
func TestOpeningSummaryCapability_OfferedForTriggerInputOnly(t *testing.T) {
	cap := openingSummaryCapability{}

	// github input → offered
	tools, skip := cap.Offer(OfferContext{Binding: &spiceboxv1alpha1.ChannelBinding{Kind: "github", Name: "gh"}})
	assert.Nil(t, skip)
	assert.Len(t, tools, 1, "a triggered input gets the enrichment tool")

	// human Slack input → not offered (this is the user's constraint)
	tools, skip = cap.Offer(OfferContext{Binding: &spiceboxv1alpha1.ChannelBinding{Kind: "slack", Name: "eng"}})
	assert.Nil(t, skip)
	assert.Empty(t, tools, "a human-started Slack session must not get the tool")

	// not channel-attached → not offered
	tools, _ = cap.Offer(OfferContext{Binding: nil})
	assert.Empty(t, tools)
}
