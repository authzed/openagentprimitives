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

// TestSelfMentionOf: the agent's own token is derived only when the input
// binding's own kind has a mentionable identity and the binding carries it —
// and from nothing else, so no capability grant can switch it off, and no
// other channel the session is bound to can stand in for the input's kind.
func TestSelfMentionOf(t *testing.T) {
	bot := map[string]string{"bot_user_id": "U0BOT"}
	// The lookup stands in for the channel-kind registry. It knows exactly two
	// kinds, so "unregistered" is a real miss rather than a nil argument.
	kinds := map[string]channelkinds.Kind{"slack": &slack.Kind{}, "fake": fake.Kind{}}
	kindByName := func(name string) (channelkinds.Kind, bool) {
		k, ok := kinds[name]
		return k, ok
	}
	cases := []struct {
		name    string
		binding *spiceboxv1alpha1.ChannelBinding
		want    string
	}{
		{name: "slack input with bot_user_id: the bot's token", binding: &spiceboxv1alpha1.ChannelBinding{Kind: "slack", External: bot}, want: "<@U0BOT>"},
		{name: "slack input without bot_user_id: no token", binding: &spiceboxv1alpha1.ChannelBinding{Kind: "slack"}, want: ""},
		{name: "input kind with no self identity: no token", binding: &spiceboxv1alpha1.ChannelBinding{Kind: "fake", External: bot}, want: ""},
		{name: "not channel-attached: no token", binding: nil, want: ""},
		{name: "input kind not registered: no token", binding: &spiceboxv1alpha1.ChannelBinding{Kind: "no-such-kind", External: bot}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, runner.SelfMentionOf(kindByName, tc.binding))
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
