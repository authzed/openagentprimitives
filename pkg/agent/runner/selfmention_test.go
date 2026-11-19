package runner_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// TestSelfMentionOf: the agent's own token is derived only when the bound kind
// has a mentionable identity and the binding carries it — and from nothing
// else, so no capability grant can switch it off.
func TestSelfMentionOf(t *testing.T) {
	bot := map[string]string{"bot_user_id": "U0BOT"}
	cases := []struct {
		name    string
		kind    channelkinds.Kind
		binding *spiceboxv1alpha1.ChannelBinding
		want    string
	}{
		{name: "slack with bot_user_id: the bot's token", kind: &slack.Kind{}, binding: &spiceboxv1alpha1.ChannelBinding{External: bot}, want: "<@U0BOT>"},
		{name: "slack without bot_user_id: no token", kind: &slack.Kind{}, binding: &spiceboxv1alpha1.ChannelBinding{}, want: ""},
		{name: "kind with no self identity: no token", kind: fake.Kind{}, binding: &spiceboxv1alpha1.ChannelBinding{External: bot}, want: ""},
		{name: "not channel-attached: no token", kind: &slack.Kind{}, binding: nil, want: ""},
		{name: "kind did not resolve: no token", kind: nil, binding: &spiceboxv1alpha1.ChannelBinding{External: bot}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, runner.SelfMentionOf(tc.kind, tc.binding))
		})
	}
}

// TestComposeSystem_SelfMentionSectionNeedsNoTool pins the reason the section
// is not a capability's: a summoning message opens with the agent's own token
// whether or not lookup_user_for_mention is offered, so the warning must reach
// the prompt of a session that was offered no tool at all.
func TestComposeSystem_SelfMentionSectionNeedsNoTool(t *testing.T) {
	got := runner.ComposeSystem("USER-PROMPT-MARKER", nil, nil, nil, nil, nil,
		runner.WithSelfMention("<@U0BOT>"),
		runner.WithPromptSections([]capability.PromptSection{{Title: "Your page", Body: "PAGE-BODY"}}))

	iSelf := strings.Index(got, "\n\n## Your mention token\n\nOn this channel you yourself are <@U0BOT>.")
	iPage := strings.Index(got, "## Your page")
	iUser := strings.Index(got, "\n---\n\nUSER-PROMPT-MARKER")
	require.NotEqual(t, -1, iSelf, "the section renders as a ## heading, a blank line, then its body:\n%s", got)
	require.NotEqual(t, -1, iPage)
	require.NotEqual(t, -1, iUser)
	assert.Contains(t, got, "never put it in a reply")
	assert.Less(t, iSelf, iPage, "the self-mention section precedes the capability sections")
	assert.Less(t, iPage, iUser, "and both precede the divider and the class prompt")
}

func TestComposeSystem_NoSelfMentionSectionWithoutAToken(t *testing.T) {
	plain := runner.ComposeSystem("hi", nil, nil, nil, nil, nil)
	assert.NotContains(t, plain, "## Your mention token")
	empty := runner.ComposeSystem("hi", nil, nil, nil, nil, nil, runner.WithSelfMention(""))
	assert.Equal(t, plain, empty, "an empty token is byte-identical to no option at all")
}
