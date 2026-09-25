package slack

import (
	"context"
	"fmt"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

var _ channelkinds.ChannelHistoryReader = (*Kind)(nil)

// channelHistoryMaxLookback caps how far back a Slack channel-history read may
// reach. Slack retains channel history far longer, but ~30 days is the product
// default; operators narrow it via spec.channelHistory.maxLookback (the
// responder clamps to the min of the two).
const (
	channelHistoryMaxLookback = 30 * 24 * time.Hour
	channelHistoryMaxMessages = 1000
)

// ChannelHistoryBounds implements channelkinds.ChannelHistoryReader.
func (k *Kind) ChannelHistoryBounds() channelkinds.ChannelHistoryBounds {
	return channelkinds.ChannelHistoryBounds{
		MaxLookback:       channelHistoryMaxLookback,
		MaxMessages:       channelHistoryMaxMessages,
		SupportsDateRange: true,
	}
}

// ChannelViewSubjectRef returns the SpiceDB subject-set of viewers of the
// channel this binding addresses. Matches the object the audience resolver and
// the external Slack membership connector use (slack_channel:<id>#view).
func (k *Kind) ChannelViewSubjectRef(binding *spiceboxv1alpha1.ChannelBinding) (string, bool) {
	id := slackChannelIDFromBinding(binding)
	if id == "" {
		return "", false
	}
	return "slack_channel:" + id + "#view", true
}

// ReadChannelHistory implements channelkinds.ChannelHistoryReader for Slack via
// conversations.history over the whole channel (contrast ReadHistory, which is
// thread-scoped via conversations.replies).
func (k *Kind) ReadChannelHistory(
	ctx context.Context, deps channelkinds.Deps,
	binding *spiceboxv1alpha1.ChannelBinding, opts channelkinds.ChannelHistoryOpts,
) (channelkinds.HistoryPage, error) {
	channelID := slackChannelIDFromBinding(binding)
	if channelID == "" {
		return channelkinds.HistoryPage{}, fmt.Errorf("slack ReadChannelHistory: no channel_id on binding")
	}
	cli := historyClientFactory(deps)
	if cli == nil {
		return channelkinds.HistoryPage{}, fmt.Errorf("slack ReadChannelHistory: no API client (credentials Secret missing bot-token?)")
	}
	params := &slackapi.GetConversationHistoryParameters{
		ChannelID: channelID,
		Limit:     opts.Limit,
		Cursor:    opts.BeforeCursor,
		Inclusive: false,
	}
	if !opts.NotBefore.IsZero() {
		params.Oldest = slackTS(opts.NotBefore)
	}
	if !opts.NotAfter.IsZero() {
		params.Latest = slackTS(opts.NotAfter)
	}
	resp, err := cli.GetConversationHistoryContext(ctx, params)
	if err != nil {
		return channelkinds.HistoryPage{}, fmt.Errorf("slack conversations.history: %w", err)
	}
	// conversations.history returns newest-first; reverse to oldest-first to
	// match HistoryPage's chronological contract.
	msgs := make([]slackapi.Message, len(resp.Messages))
	for i, m := range resp.Messages {
		msgs[len(resp.Messages)-1-i] = m
	}
	return channelkinds.HistoryPage{
		Messages:   resolveAuthors(ctx, cli, msgs, selfBotUserID(deps), k.resolveInstalledTeamID(ctx, cli)),
		HasMore:    resp.HasMore,
		NextCursor: resp.ResponseMetaData.NextCursor,
	}, nil
}

// slackChannelIDFromBinding extracts the Slack channel id: External["channel_id"]
// first, falling back to parsing "thread:<channel_id>:<thread_ts>" from Key.
func slackChannelIDFromBinding(b *spiceboxv1alpha1.ChannelBinding) string {
	if b == nil {
		return ""
	}
	if id := b.External["channel_id"]; id != "" {
		return id
	}
	if rest, ok := strings.CutPrefix(b.Key, "thread:"); ok {
		if id, _, ok := strings.Cut(rest, ":"); ok {
			return id
		}
	}
	return ""
}

// slackTS renders a time as a Slack ts string ("<unixseconds>.000000").
func slackTS(t time.Time) string { return fmt.Sprintf("%d.000000", t.Unix()) }
