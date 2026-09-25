//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
)

// TestSlackConversationHelpers_SelfContained exercises SendSlackDM,
// SlackTopLevel, and ExpectSlackThreadedReply directly against a
// hand-built Harness — no envtest/SpiceDB/manager boot required, since
// this task's job is the harness-side plumbing (inject → push → poll →
// assert), not the real listener/pipeline/runner/sender wiring a full
// kind: slack Channel fixture would exercise end to end (that lands in
// Task 6, once testdata exists). This test proves:
//
//  1. SendSlackDM records the DM in fakeslack's conversation tree AND
//     pushes a consumable socketmode.Event onto the source (i.e. what a
//     real listener's dispatch loop would drain).
//  2. SlackTopLevel surfaces that DM.
//  3. ExpectSlackThreadedReply finds a reply posted with thread_ts ==
//     the DM's ts — simulating what the real slack sender does on
//     reply (see sender.go's slackapi.MsgOptionTS(threadTS)) — and
//     returns it promptly.
func TestSlackConversationHelpers_SelfContained(t *testing.T) {
	h := &Harness{
		t:           t,
		slackFake:   fakeslack.New(),
		slackSource: fakeslack.NewSocketSource(),
	}

	const userID, channelID = "U1", "C1"
	rootTS := h.SendSlackDM(userID, channelID, "hello")
	require.NotEmpty(t, rootTS, "SendSlackDM must return a non-empty ts")

	// Drain the pushed event the way the real listener's dispatch loop
	// would (nothing else is consuming h.slackSource here), proving Push
	// actually enqueued the injected DM in the shape listener.go expects
	// (see fakeslack.InjectDM's godoc).
	select {
	case ev := <-h.slackSource.Events():
		assert.NotNil(t, ev.Request, "pushed event must carry a non-nil socketmode.Request")
	case <-time.After(time.Second):
		t.Fatal("SendSlackDM: no event observed on slackSource.Events() within 1s")
	}

	top := h.SlackTopLevel(channelID)
	require.Len(t, top, 1, "the injected DM must be recorded as a top-level message")
	assert.Equal(t, rootTS, top[0].TS)
	assert.Equal(t, "hello", top[0].Text)

	// No reply yet.
	assert.Empty(t, h.slackFake.Replies(channelID, rootTS))

	// Simulate the real slack sender's reply: PostMessageContext with
	// thread_ts == rootTS.
	_, _, err := h.slackFake.PostMessageContext(context.Background(), channelID,
		slackapi.MsgOptionText("hi back", false), slackapi.MsgOptionTS(rootTS))
	require.NoError(t, err)

	reply := h.ExpectSlackThreadedReply(t, channelID, rootTS, 2*time.Second)
	assert.Equal(t, "hi back", reply.Text)
	assert.Equal(t, rootTS, reply.ThreadTS)
}
