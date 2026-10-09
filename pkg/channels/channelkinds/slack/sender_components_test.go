package slack

import (
	"context"
	"encoding/json"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func userMessageEnvelopeWithComponents(t *testing.T, text string, components json.RawMessage) channelevents.Envelope {
	t.Helper()
	pl, err := json.Marshal(channelevents.OutboundUserMessagePayload{Text: text, Components: components})
	require.NoError(t, err, "marshal payload")
	return channelevents.Envelope{
		Version: 1,
		Kind:    channelevents.KindUserMessage,
		Session: channelevents.SessionRef{Namespace: "default", Name: "foo"},
		Payload: pl,
	}
}

// Agent-authored components become the message body; the text field is the
// notification fallback, not a duplicated body section.
func TestBuildUserMessageBlocks_ComponentsAreTheBody(t *testing.T) {
	s := newSender(&fakeSlackClient{})
	sess := channelkinds.SessionInfo{Namespace: "default", Name: "foo"}
	components := json.RawMessage(`[{"type":"header","text":{"type":"plain_text","text":"Results"}},{"type":"section","text":{"type":"mrkdwn","text":"*all good*"}}]`)

	blocks := s.buildUserMessageBlocks(context.Background(), sess, "plain summary", components, "default/foo", false, noopInfoLogger{})

	var headers, sections int
	for _, b := range blocks {
		switch b.(type) {
		case *slackapi.HeaderBlock:
			headers++
		case *slackapi.SectionBlock:
			sections++
		}
	}
	assert.Equal(t, 1, headers, "the agent's header block must render")
	assert.GreaterOrEqual(t, sections, 1, "the agent's section block must render")
	assert.NotContains(t, concatBlockText(blocks), "plain summary",
		"the text field is the notification fallback, not a body section, when components are present")
	assert.Contains(t, concatBlockText(blocks), "Results", "the agent's header content must be present")
}

// Defense in depth: if components somehow fail to parse at render time (they
// were validated in the runner), the content is never dropped — the sender
// falls back to the text field as a section.
func TestBuildUserMessageBlocks_UnparseableComponentsFallBackToText(t *testing.T) {
	s := newSender(&fakeSlackClient{})
	sess := channelkinds.SessionInfo{Namespace: "default", Name: "foo"}

	blocks := s.buildUserMessageBlocks(context.Background(), sess, "the summary text", json.RawMessage(`not valid json`), "default/foo", false, noopInfoLogger{})
	assert.Contains(t, concatBlockText(blocks), "the summary text",
		"unparseable components must fall back to the text section so nothing is lost")
}

// Absent components, behavior is unchanged: the text renders as section blocks.
func TestBuildUserMessageBlocks_NoComponentsUsesText(t *testing.T) {
	s := newSender(&fakeSlackClient{})
	sess := channelkinds.SessionInfo{Namespace: "default", Name: "foo"}

	blocks := s.buildUserMessageBlocks(context.Background(), sess, "hello there", nil, "default/foo", false, noopInfoLogger{})
	assert.Contains(t, concatBlockText(blocks), "hello there", "text renders as a section when no components are present")
}

// End to end through Send: the post carries Block Kit (Text + Blocks options)
// and the text is the notification fallback. (Block *content* is asserted by
// TestBuildUserMessageBlocks_ComponentsAreTheBody; slack-go does not expose the
// resolved blocks through a MsgOption, so the wire test asserts presence via the
// option shape — the same way the degrade test distinguishes block vs plain-text
// posts.)
func TestSenderUserMessage_ComponentsOnWire(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF"},
		},
	}
	components := json.RawMessage(`[{"type":"header","text":{"type":"plain_text","text":"Quarterly Results"}}]`)
	_, err := s.Send(context.Background(), sess, userMessageEnvelopeWithComponents(t, "summary fallback", components))
	require.NoError(t, err)
	require.Len(t, c.postMessageCalls, 1, "one post")

	opts := c.postMessageCalls[0].options
	assert.Len(t, opts, 2, "a components reply posts Text + Blocks (no thread_ts), not a degraded plain-text post")
	assert.Equal(t, "summary fallback", renderTextFromOpts(t, opts), "text is the notification fallback")
}
