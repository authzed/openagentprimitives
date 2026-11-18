package capability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// TestMentionLookup_SelfMentionSection: the agent is told its own token only
// when the bound kind can derive one from the binding — a summoning message
// opens with that token, and unexplained it reads as the person to address.
func TestMentionLookup_SelfMentionSection(t *testing.T) {
	cases := []struct {
		name        string
		kind        channelkinds.Kind
		external    map[string]string
		wantSection bool
	}{
		{name: "slack with bot_user_id: section names the bot's token", kind: &slack.Kind{}, external: map[string]string{"bot_user_id": "U0BOT"}, wantSection: true},
		{name: "slack without bot_user_id: no section", kind: &slack.Kind{}, external: nil, wantSection: false},
		{name: "kind with no self identity: no section", kind: fake.Kind{}, external: map[string]string{"bot_user_id": "U0BOT"}, wantSection: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, sections, skip := mentionLookupCapability{}.OfferWithSections(OfferContext{
				Ctx:     context.Background(),
				Binding: &spiceboxv1alpha1.ChannelBinding{External: tc.external},
				Env: RunnerEnv{
					ResolvedChannel: &spiceboxv1alpha1.Channel{Spec: spiceboxv1alpha1.ChannelSpec{Kind: "test"}},
					ResolvedKind:    tc.kind,
				},
			})
			require.Nil(t, skip)
			if !tc.wantSection {
				assert.Empty(t, sections)
				return
			}
			require.Len(t, sections, 1)
			assert.Equal(t, selfMentionSectionTitle, sections[0].Title)
			assert.Contains(t, sections[0].Body, "you yourself are <@U0BOT>")
			assert.Contains(t, sections[0].Body, "never put it in a reply")
		})
	}
}

// TestMentionLookup_NotChannelAttached_NoSection: no binding, no tool and no
// section — the same gate for both, so they cannot disagree.
func TestMentionLookup_NotChannelAttached_NoSection(t *testing.T) {
	tools, sections, skip := mentionLookupCapability{}.OfferWithSections(OfferContext{Ctx: context.Background()})
	assert.Nil(t, skip)
	assert.Empty(t, tools)
	assert.Empty(t, sections)
}
