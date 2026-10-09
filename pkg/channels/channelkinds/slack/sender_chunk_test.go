package slack

import (
	"context"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// noopInfoLogger satisfies the minimal logging interface buildUserMessageBlocks
// expects without pulling in a real logr sink.
type noopInfoLogger struct{}

func (noopInfoLogger) Info(string, ...any) {}

// TestChunkForSlackSection verifies the content-preserving splitter that keeps
// each Slack section-block mrkdwn payload under the 3000-char limit. The core
// invariant is that NO content is ever dropped: concatenating the chunks
// reproduces the input byte-for-byte.
func TestChunkForSlackSection(t *testing.T) {
	cases := []struct {
		name string
		text string
		max  int
		want []string // when non-nil, asserts the exact chunk boundaries
	}{
		{
			name: "short text under the limit is a single verbatim chunk",
			text: "hello world",
			max:  100,
			want: []string{"hello world"},
		},
		{
			name: "empty text is preserved as a single empty chunk (no regression)",
			text: "",
			max:  100,
			want: []string{""},
		},
		{
			name: "text exactly at the limit stays a single chunk",
			text: strings.Repeat("a", 100),
			max:  100,
			want: []string{strings.Repeat("a", 100)},
		},
		{
			name: "breakable text splits on the last whitespace before the limit",
			text: "aaaaa bbbbb ccccc",
			max:  8,
			want: []string{"aaaaa ", "bbbbb ", "ccccc"},
		},
		{
			name: "unbreakable token is hard-cut into limit-sized pieces",
			text: strings.Repeat("x", 250),
			max:  100,
			want: []string{strings.Repeat("x", 100), strings.Repeat("x", 100), strings.Repeat("x", 50)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkForSlackSection(tc.text, tc.max)

			// Invariant 1: never drop content — the join is exact.
			assert.Equal(t, tc.text, strings.Join(got, ""), "concatenated chunks must reproduce the input exactly")

			// Invariant 2: every chunk respects the limit.
			for i, c := range got {
				assert.LessOrEqual(t, len([]rune(c)), tc.max, "chunk %d exceeds the rune limit", i)
			}

			if tc.want != nil {
				assert.Equal(t, tc.want, got, "chunk boundaries")
			}
		})
	}
}

// TestChunkForSlackSection_LongReplyStaysUnderSlackLimit exercises the real
// failure mode from production: a multi-thousand-character agent reply that a
// single section block would reject with invalid_blocks.
func TestChunkForSlackSection_LongReplyStaysUnderSlackLimit(t *testing.T) {
	// ~7500 runes of realistic prose (well over Slack's 3000-char section cap).
	para := "Drive qualified traffic to the RAG demo with copy that leads on trust. "
	long := strings.Repeat(para, 110)
	require.Greater(t, len([]rune(long)), 3000, "fixture must exceed the section limit")

	got := chunkForSlackSection(long, slackSectionMaxRunes)

	require.GreaterOrEqual(t, len(got), 2, "an over-limit reply must split into multiple chunks")
	assert.Equal(t, long, strings.Join(got, ""), "no content dropped")
	for i, c := range got {
		assert.LessOrEqual(t, len([]rune(c)), slackSectionMaxRunes, "chunk %d exceeds slackSectionMaxRunes", i)
	}
}

// TestBuildUserMessageBlocks_LongReply_SplitsIntoSectionBlocksUnderLimit is the
// root-cause regression: a long agent reply must render as multiple section
// blocks, each under Slack's 3000-char cap, with all content preserved and the
// Show-settings action block still present — rather than one oversized section
// block that Slack rejects with invalid_blocks.
func TestBuildUserMessageBlocks_LongReply_SplitsIntoSectionBlocksUnderLimit(t *testing.T) {
	s := newSender(&fakeSlackClient{})
	sess := channelkinds.SessionInfo{Namespace: "default", Name: "foo"}

	// A realistic over-limit reply (LinkedIn ad copy, 5+ options + graphics).
	long := strings.Repeat("Option: lead with trust, close with the RAG demo link. ", 120)
	require.Greater(t, len([]rune(long)), 3000, "fixture must exceed the section limit")

	blocks := s.buildUserMessageBlocks(context.Background(), sess, long, nil, "default/foo", true, noopInfoLogger{})

	var sections []*slackapi.SectionBlock
	var actions int
	for _, b := range blocks {
		switch tb := b.(type) {
		case *slackapi.SectionBlock:
			sections = append(sections, tb)
		case *slackapi.ActionBlock:
			actions++
		}
	}

	require.GreaterOrEqual(t, len(sections), 2, "long reply must span multiple section blocks")
	assert.Equal(t, 1, actions, "exactly one Show-settings action block")
	assert.LessOrEqual(t, len(blocks), slackMaxBlocksPerMessage, "must stay within Slack's per-message block limit")

	var joined strings.Builder
	for _, sec := range sections {
		require.NotNil(t, sec.Text, "section block must carry text")
		assert.LessOrEqual(t, len([]rune(sec.Text.Text)), slackSectionMaxRunes,
			"each section block must stay under the mrkdwn limit")
		joined.WriteString(sec.Text.Text)
	}
	assert.Equal(t, long, joined.String(), "the reply content must be fully preserved across section blocks")
}

// TestSenderUserMessage_InvalidBlocks_RetriesAsPlainText covers the defense-in-
// depth net: when the block-rendered post is still rejected with invalid_blocks
// (e.g. an unbreakable >2900-char token, or block-count overflow), the sender
// must re-post the reply as a plain-text message — delivering the content
// instead of dropping it and returning an error.
func TestSenderUserMessage_InvalidBlocks_RetriesAsPlainText(t *testing.T) {
	c := &fakeSlackClient{
		// First (block) post is rejected; the plain-text retry succeeds.
		postMessageErrs: []error{slackapi.SlackErrorResponse{Err: "invalid_blocks"}, nil},
	}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{
				"channel_id": "C01ABCDEF",
				"thread_ts":  "1614191050.013300",
			},
		},
	}

	_, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "the full agent reply"))
	require.NoError(t, err, "invalid_blocks must be recovered by the plain-text retry, not surfaced as an error")

	require.Len(t, c.postMessageCalls, 2, "expected the block post plus a plain-text retry")
	assert.Len(t, c.postMessageCalls[0].options, 3, "first post carries Text + Blocks + TS")
	assert.Len(t, c.postMessageCalls[1].options, 2, "plain-text retry carries Text + TS, no Blocks")
	assert.Equal(t, "the full agent reply", renderTextFromOpts(t, c.postMessageCalls[1].options),
		"the retry must carry the full reply text")
}

// TestSenderUserMessage_NonBlockError_Propagates guards against the fallback
// swallowing unrelated failures: a rate-limit error is not invalid_blocks, so
// it must propagate without a retry.
func TestSenderUserMessage_NonBlockError_Propagates(t *testing.T) {
	c := &fakeSlackClient{
		postMessageErrs: []error{slackapi.SlackErrorResponse{Err: "rate_limited"}},
	}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF"},
		},
	}

	_, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "hi"))
	require.Error(t, err, "a non-block error must propagate")
	assert.Contains(t, err.Error(), "rate_limited", "error message")
	assert.Len(t, c.postMessageCalls, 1, "no retry for non-block errors")
}
