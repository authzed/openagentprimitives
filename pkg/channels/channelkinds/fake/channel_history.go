package fake

import (
	"context"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

var _ channelkinds.ChannelHistoryReader = (*Kind)(nil)

// fakeChannelHistoryMessages are the canned whole-channel history messages
// ReadChannelHistory returns, oldest-first. NEVER use in production — like
// the rest of this package, the fake kind exists for tests only (see the
// package doc).
var fakeChannelHistoryMessages = []channelkinds.HistoryMessage{
	{AuthorExternalID: "alice", AuthorDisplayName: "alice", Text: "hello", TS: "1.0"},
	{AuthorExternalID: "bob", AuthorDisplayName: "bob", Text: "world", TS: "2.0"},
	{AuthorExternalID: "carol", AuthorDisplayName: "carol", Text: "how's it going?", TS: "3.0"},
}

// ChannelHistoryBounds implements channelkinds.ChannelHistoryReader with
// arbitrary but plausible test bounds — the fake kind has no real message
// store to enforce them against.
func (Kind) ChannelHistoryBounds() channelkinds.ChannelHistoryBounds {
	return channelkinds.ChannelHistoryBounds{
		MaxLookback:       30 * 24 * time.Hour,
		MaxMessages:       100,
		SupportsDateRange: false,
	}
}

// ReadChannelHistory implements channelkinds.ChannelHistoryReader. Returns
// the fixed canned page above — the fake kind has no real per-channel
// message store to read from — lightly honoring opts.Limit (returning the
// most recent Limit messages) so pagination-shaped e2e assertions have
// something to observe. Every other opts field (time bounds, cursor) is
// ignored: there is nothing to page through beyond the fixed set.
func (Kind) ReadChannelHistory(
	_ context.Context, _ channelkinds.Deps, _ *spiceboxv1alpha1.ChannelBinding, opts channelkinds.ChannelHistoryOpts,
) (channelkinds.HistoryPage, error) {
	msgs := fakeChannelHistoryMessages
	if opts.Limit > 0 && opts.Limit < len(msgs) {
		msgs = msgs[len(msgs)-opts.Limit:]
	}
	out := make([]channelkinds.HistoryMessage, len(msgs))
	copy(out, msgs)
	return channelkinds.HistoryPage{Messages: out}, nil
}

// ChannelViewSubjectRef implements channelkinds.ChannelHistoryReader,
// mirroring the slack kind's "<kind>_channel:<id>#view" shape so
// info-leakage-on e2e scenarios can exercise the SpiceDB check path. Returns
// ok=false when the binding carries no channel_id — the fake driver's
// SendUserMessage-injected inbound events don't set one, so tests relying
// on this ref must inject External["channel_id"] explicitly.
func (Kind) ChannelViewSubjectRef(binding *spiceboxv1alpha1.ChannelBinding) (string, bool) {
	if binding == nil {
		return "", false
	}
	id := binding.External["channel_id"]
	if id == "" {
		return "", false
	}
	return "fake_channel:" + id + "#view", true
}
