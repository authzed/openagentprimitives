// pkg/channels/channelkinds/slack/thread_placement_test.go
//
// Tree-based placement proofs for the DM-threading fix: agent replies must
// land as threaded children of the anchor message (the user's inbound for
// DMs, the established anchor for channels), never as a second top-level
// message. Uses fakeslack's message-tree query API (TopLevel/Replies) rather
// than inspecting opaque MsgOptions, so these tests assert WHERE a message
// landed, not just that a thread_ts string was passed somewhere.
package slack

import (
	"context"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
)

// dmSession builds a DM SessionInfo whose per-turn anchor (LastInboundTS) is
// rootTS — i.e. the user's message the reply should thread under.
func dmSession(channelID, rootTS string) channelkinds.SessionInfo {
	return channelkinds.SessionInfo{
		Namespace: "ns", Name: "sess",
		Channel:     &v1alpha1.ChannelBinding{Kind: "slack", External: map[string]string{"channel_id": channelID}},
		Annotations: map[string]string{LastInboundTSAnnotationKey: rootTS},
	}
}

func TestDMReply_ThreadsUnderUserMessage(t *testing.T) {
	fake := fakeslack.New()
	ctx := context.Background()
	// Simulate the user's inbound DM as the thread root.
	_, rootTS, err := fake.PostMessageContext(ctx, "D01", slackapi.MsgOptionText("do the thing", false))
	require.NoError(t, err)

	s := &slackSender{client: fake} // direct construction; nil streamSink ⇒ final-post path
	env := userMessageEnvelope(t, "done!")
	_, err = s.Send(ctx, dmSession("D01", rootTS), env)
	require.NoError(t, err)

	// The agent reply must be a threaded child of the user's message, NOT a
	// second top-level message. This is the exact regression from the bug.
	assert.Len(t, fake.TopLevel("D01"), 1, "only the user message is top-level")
	replies := fake.Replies("D01", rootTS)
	require.Len(t, replies, 1, "agent reply must thread under the user message")
	assert.Contains(t, replies[0].Text, "done!")
}

func TestChannelReply_StillThreadsUnderAnchor(t *testing.T) {
	// Guard existing channel behavior: a channel binding carries thread_ts on
	// External (set at inbound). No annotation needed; must still thread.
	fake := fakeslack.New()
	ctx := context.Background()
	_, anchor, err := fake.PostMessageContext(ctx, "C01", slackapi.MsgOptionText("@bot help", false))
	require.NoError(t, err)

	sess := channelkinds.SessionInfo{
		Namespace: "ns", Name: "sess",
		Channel: &v1alpha1.ChannelBinding{Kind: "slack", External: map[string]string{"channel_id": "C01", "thread_ts": anchor}},
	}
	s := &slackSender{client: fake}
	_, err = s.Send(ctx, sess, userMessageEnvelope(t, "here you go"))
	require.NoError(t, err)

	require.Len(t, fake.Replies("C01", anchor), 1, "channel reply must still thread under the anchor")
}

// streamDelta drives a single text_delta through the sink and sleeps past
// the configured debounce so the streaming bubble flush (Poster.PostMessage
// or Updater.UpdateMessage) actually fires before the test inspects state —
// mirroring the sleep-past-debounce pattern in stream_delta_sink_test.go.
func streamDelta(t *testing.T, sink *StreamDeltaSink, sess channelkinds.SessionInfo, text string) {
	t.Helper()
	require.NoError(t, sink.OnDelta(context.Background(), sess, deltaEnv(t, "text_delta", text, "")))
	time.Sleep(80 * time.Millisecond)
}

// streamStop drives a "stop" event, finalizing the streaming bubble ts
// (moving threadState.placeholder → finalizedTS) so ConsumeFinalTarget can
// retrieve it.
func streamStop(t *testing.T, sink *StreamDeltaSink, sess channelkinds.SessionInfo) {
	t.Helper()
	require.NoError(t, sink.OnDelta(context.Background(), sess, deltaEnv(t, "stop", "", "")))
}

// TestDMStreaming_BubbleAndFinalAreOneThreadedMessage proves the bubble +
// final land as ONE threaded message (no orphan top-level bubble): the
// stream-delta sink and the sender must resolve the SAME effective
// thread_ts for a turn, or the sender's ConsumeFinalTarget lookup misses
// and double-posts.
func TestDMStreaming_BubbleAndFinalAreOneThreadedMessage(t *testing.T) {
	fake := fakeslack.New()
	ctx := context.Background()
	_, rootTS, err := fake.PostMessageContext(ctx, "D01", slackapi.MsgOptionText("stream please", false))
	require.NoError(t, err)

	sink := NewStreamDeltaSink(StreamDeltaSinkConfig{
		Poster:         &SlackPoster{Client: fake},
		Updater:        &SlackUpdater{Client: fake},
		DebounceWindow: 20 * time.Millisecond,
	})
	defer sink.Close()
	sess := dmSession("D01", rootTS)
	// drive a text_delta then a stop, flushing the bubble.
	streamDelta(t, sink, sess, "partial ")
	streamStop(t, sink, sess)

	replies := fake.Replies("D01", rootTS)
	require.Len(t, replies, 1, "exactly one agent message in the thread (the streamed bubble)")
	assert.Equal(t, rootTS, replies[0].ThreadTS)
	// The sender consuming the bubble targets the SAME thread_ts, so it updates
	// (not double-posts): ConsumeFinalTarget must hit.
	assert.NotEmpty(t, sink.ConsumeFinalTarget("D01", rootTS), "final target resolves to the bubble's thread")
}
