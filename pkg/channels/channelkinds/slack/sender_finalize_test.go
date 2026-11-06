package slack

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// A session that ends without the runner closing out its plan leaves the card's
// in-progress item drawn as an hourglass. FinalizeSession re-renders exactly
// those cards as stopped, leaves settled ones alone, and forgets them all.
func TestFinalizeSession_StopsOnlyLiveCards(t *testing.T) {
	sess := sessForPlanTest()
	other := sessForPlanTest()
	other.Name = "sess2"

	cases := []struct {
		name       string
		items      []channelevents.PlanItemRef
		paused     bool
		failed     bool
		wantUpdate bool
		wantBanner string
	}{
		{
			name:       "in-progress card on a failed session: re-rendered with the error banner",
			items:      []channelevents.PlanItemRef{{ID: "a", Label: "Review", Status: "in_progress"}},
			failed:     true,
			wantUpdate: true,
			wantBanner: "hit an error",
		},
		{
			name:       "in-progress card on a succeeded session: re-rendered as interrupted",
			items:      []channelevents.PlanItemRef{{ID: "a", Label: "Review", Status: "in_progress"}},
			wantUpdate: true,
			wantBanner: "interrupted",
		},
		{
			name:  "finished card: left alone",
			items: []channelevents.PlanItemRef{{ID: "a", Label: "Review", Status: "done"}},
		},
		{
			name:   "card the runner already paused: left alone",
			items:  []channelevents.PlanItemRef{{ID: "a", Label: "Review", Status: "in_progress"}},
			paused: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeSlackClient{postedTS: "1700000000.000100"}
			s := newSender(c)
			pl := channelevents.PlanUpdatePayload{PlanName: "main", Items: tc.items, Paused: tc.paused}
			if tc.paused {
				pl.PauseCause = channelevents.PauseCauseIdle
			}
			_, err := s.Send(context.Background(), sess, planEnvelope(t, pl))
			require.NoError(t, err)
			// Another session's live card must never be touched.
			c.postedTS = "1700000000.000200"
			_, err = s.Send(context.Background(), other, planEnvelope(t, channelevents.PlanUpdatePayload{
				PlanName: "main", Items: []channelevents.PlanItemRef{{ID: "x", Label: "X", Status: "in_progress"}},
			}))
			require.NoError(t, err)
			before := len(c.snapshotUpdateCalls())

			require.NoError(t, s.FinalizeSession(context.Background(), sess, tc.failed))

			updates := c.snapshotUpdateCalls()[before:]
			if !tc.wantUpdate {
				assert.Empty(t, updates, "a settled card is not re-rendered")
			} else if assert.Len(t, updates, 1, "exactly the live card is re-rendered") {
				assert.Equal(t, "1700000000.000100", updates[0].ts)
				assert.Contains(t, msgOptionBlocksJSON(t, updates[0].opts), tc.wantBanner)
			}
			assert.False(t, s.planMessages.has(sess, "main"), "the ended session's cards are forgotten")
			assert.True(t, s.planMessages.has(other, "main"), "another session's cards are kept")

			// Idempotent: a second call finds nothing to do.
			require.NoError(t, s.FinalizeSession(context.Background(), sess, tc.failed))
			assert.Len(t, c.snapshotUpdateCalls(), before+len(updates))
		})
	}
}

// A card whose re-render FAILS stays tracked, so the watcher's retry on the next
// tick re-renders it. Dropping it before the call would turn the retry into a
// silent success and leave the hourglass on a dead session for good.
func TestFinalizeSession_FailedUpdateIsRetried(t *testing.T) {
	sess := sessForPlanTest()
	c := &fakeSlackClient{postedTS: "1700000000.000100"}
	s := newSender(c)
	_, err := s.Send(context.Background(), sess, planEnvelope(t, channelevents.PlanUpdatePayload{
		PlanName: "main", Items: []channelevents.PlanItemRef{{ID: "a", Label: "Review", Status: "in_progress"}},
	}))
	require.NoError(t, err)
	before := len(c.snapshotUpdateCalls())

	c.mu.Lock()
	c.updateErr = errors.New("ratelimited")
	c.mu.Unlock()
	require.Error(t, s.FinalizeSession(context.Background(), sess, true), "the failed re-render is reported")
	assert.True(t, s.planMessages.has(sess, "main"), "a card that failed to re-render is kept for the retry")

	c.mu.Lock()
	c.updateErr = nil
	c.mu.Unlock()
	require.NoError(t, s.FinalizeSession(context.Background(), sess, true))
	updates := c.snapshotUpdateCalls()[before:]
	require.Len(t, updates, 2, "the retry re-renders the card again")
	assert.Equal(t, "1700000000.000100", updates[1].ts)
	assert.False(t, s.planMessages.has(sess, "main"), "forgotten once re-rendered")
}
