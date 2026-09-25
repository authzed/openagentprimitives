package slack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// newQueuedListener builds a listener whose pipeline reports "routed, but
// queued behind the live turn" — the decision Deliver returns for an inbound
// that arrives while the session is Running.
func newQueuedListener(t *testing.T, api listenerAPIClient, queued bool) *slackListener {
	t.Helper()
	l := &slackListener{
		deps: channelkinds.Deps{
			Inbound: fixedInbound{dec: channelkinds.InboundDecision{
				Outcome: channelkinds.OutcomeRouted,
				Session: channelkinds.SessionInfo{Namespace: "default", Name: "slack-ch-abc"},
				Queued:  queued,
			}},
		},
		api:              api,
		seenEvts:         newEventIDCache(64),
		threads:          newThreadIndex(),
		assistantThreads: map[string]string{},
		botUserID:        "UBOT",
		idents:           NewIdentityCache(8),
		installedTeamID:  "T1",
	}
	// Prime the identity cache so resolveIdentity does not call users.info.
	l.idents.Put(userInfo{UserID: "U1", Email: "u1@example.com", TeamID: "T1"})
	return l
}

// TestHandleChannelMessage_QueuedInboundSetsNoStartingStatus: a thread reply
// that lands mid-turn is queued behind the running turn, so the listener must
// NOT announce "<agent> is starting…".
//
// The pipeline has already told the user "you messaged while I'm working —
// your message is queued"; a starting status contradicts that ack, and it
// overwrites the live turn's own progress caption with a claim about a turn
// that has not begun. The status line belongs to the turn in flight.
func TestHandleChannelMessage_QueuedInboundSetsNoStartingStatus(t *testing.T) {
	api, hits := recordingSlackAPI(t)
	l := newQueuedListener(t, api, true)

	l.handleChannelMessage(context.Background(), "U1", "C1", "9.9", "10.0", "are you still there?", nil, false, originHuman)

	assert.False(t, hit(hits(), "/assistant.threads.setStatus"),
		"a queued mid-turn reply must not set a starting status; hits=%v", hits())
}

// TestHandleChannelMessage_RoutedInboundStillSetsStartingStatus is the guard
// on the fix: only the queued case is suppressed. An inbound that actually
// starts a turn must keep announcing it.
func TestHandleChannelMessage_RoutedInboundStillSetsStartingStatus(t *testing.T) {
	api, hits := recordingSlackAPI(t)
	l := newQueuedListener(t, api, false)

	l.handleChannelMessage(context.Background(), "U1", "C1", "9.9", "10.0", "hello", nil, false, originHuman)

	assert.True(t, hit(hits(), "/assistant.threads.setStatus"),
		"an inbound that starts a turn must set the starting status; hits=%v", hits())
}

// TestHandleDM_QueuedInboundSetsNoStartingStatus: the DM path carries the same
// contradiction — the pipeline queues a mid-turn DM and acks it, so the DM
// branch must suppress its starting status too.
func TestHandleDM_QueuedInboundSetsNoStartingStatus(t *testing.T) {
	api, hits := recordingSlackAPI(t)
	l := newQueuedListener(t, api, true)

	l.handleDM(context.Background(), "U1", "D01ABCDEF", "1700000000.000400", "are you still there?", nil)

	assert.False(t, hit(hits(), "/assistant.threads.setStatus"),
		"a queued mid-turn DM must not set a starting status; hits=%v", hits())
}
