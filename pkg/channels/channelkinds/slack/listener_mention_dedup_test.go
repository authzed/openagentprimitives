package slack

import (
	"context"
	"sync"
	"testing"

	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
)

// countingInbound records every InboundEvent the listener delivered, so a test
// can assert how MANY times one Slack message reached the pipeline — not just
// that it reached it at all.
type countingInbound struct {
	mu   sync.Mutex
	dec  channelkinds.InboundDecision
	seen []channelkinds.InboundEvent
}

func (c *countingInbound) Deliver(_ context.Context, ev channelkinds.InboundEvent) (channelkinds.InboundDecision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, ev)
	return c.dec, nil
}

func (c *countingInbound) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

// newDedupListener builds a listener wired for the channel-message dispatch
// path with an owned thread already in the index, so threadIsOwned takes the
// fast path and no apiserver lookup is involved.
func newDedupListener(t *testing.T, inbound *countingInbound, channelID, threadTS string) *slackListener {
	t.Helper()
	channel, builder := newTestChannel(t)
	l := &slackListener{
		deps: channelkinds.Deps{
			Channel:   channel,
			K8sClient: builder.Build(),
			Inbound:   inbound,
		},
		api:              fakeslack.New(),
		seenEvts:         newEventIDCache(64),
		threads:          newThreadIndex(),
		assistantThreads: map[string]string{},
		idents:           NewIdentityCache(8),
		botUserID:        fakeslack.BotUserID,
	}
	l.threads.put(channelID+":"+threadTS, "")
	return l
}

// threadReplyMentionEvents returns the TWO Events API deliveries Slack makes
// for a single @-mention posted as a reply inside an existing thread: the
// app_mention subscription fires, and message.channels fires for the same
// message. Both carry the same (channel, ts).
func threadReplyMentionEvents(channelID, userID, threadTS, ts, text string) (appMention, message slackevents.EventsAPIEvent) {
	appMention = slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "app_mention",
			Data: &slackevents.AppMentionEvent{
				Channel:         channelID,
				User:            userID,
				ThreadTimeStamp: threadTS,
				TimeStamp:       ts,
				Text:            text,
			},
		},
	}
	message = slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "message",
			Data: &slackevents.MessageEvent{
				Channel:         channelID,
				ChannelType:     "channel",
				User:            userID,
				ThreadTimeStamp: threadTS,
				TimeStamp:       ts,
				Text:            text,
			},
		},
	}
	return appMention, message
}

// TestThreadReplyMention_DeliveredExactlyOnce is the regression guard for the
// double-delivery defect: an @-mention posted as a THREAD REPLY arrives twice
// from Slack (app_mention AND message.channels), and the two arms claimed the
// seenEvts LRU under DIFFERENT key prefixes, so neither deduped the other and
// the agent was handed the same user message twice. Nothing downstream is
// idempotent, so the user got two turns for one message.
//
// Both arrival orders are covered: Slack guarantees no ordering between the
// two subscriptions.
func TestThreadReplyMention_DeliveredExactlyOnce(t *testing.T) {
	const (
		channelID = "C01"
		userID    = "U_ALICE"
		threadTS  = "1700000000.000100"
		msgTS     = "1700000000.000200"
	)
	text := "<@" + fakeslack.BotUserID + "> follow up on that"

	cases := []struct {
		name  string
		first bool // true = app_mention arrives first
	}{
		{name: "app_mention first: exactly one delivery", first: true},
		{name: "message.channels first: exactly one delivery", first: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inbound := &countingInbound{dec: channelkinds.InboundDecision{
				Outcome: channelkinds.OutcomeRouted,
				Queued:  true, // no status write; keeps the assertion on delivery
			}}
			l := newDedupListener(t, inbound, channelID, threadTS)
			appMention, message := threadReplyMentionEvents(channelID, userID, threadTS, msgTS, text)

			ctx := context.Background()
			if tc.first {
				l.handleEventsAPI(ctx, appMention)
				l.handleEventsAPI(ctx, message)
			} else {
				l.handleEventsAPI(ctx, message)
				l.handleEventsAPI(ctx, appMention)
			}

			require.Equal(t, 1, inbound.count(),
				"one @-mention in a thread must reach the pipeline exactly once, "+
					"however Slack orders its app_mention and message.channels deliveries")
			assert.Equal(t, text, inbound.seen[0].MessageText)
		})
	}
}

// TestThreadReplyWithoutMention_StillDelivered pins the other half of the
// contract: a PLAIN thread reply (no @-mention) has no app_mention twin, so
// the message.channels arm must keep delivering it.
func TestThreadReplyWithoutMention_StillDelivered(t *testing.T) {
	const (
		channelID = "C01"
		userID    = "U_ALICE"
		threadTS  = "1700000000.000100"
		msgTS     = "1700000000.000300"
	)
	inbound := &countingInbound{dec: channelkinds.InboundDecision{
		Outcome: channelkinds.OutcomeRouted,
		Queued:  true,
	}}
	l := newDedupListener(t, inbound, channelID, threadTS)
	_, message := threadReplyMentionEvents(channelID, userID, threadTS, msgTS, "plain reply, no mention")

	l.handleEventsAPI(context.Background(), message)

	require.Equal(t, 1, inbound.count(), "a plain thread reply has no app_mention twin and must still route")
}
