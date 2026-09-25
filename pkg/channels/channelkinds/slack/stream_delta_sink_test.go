package slack

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// recordedUpdate captures one chat.update call for assertion.
type recordedUpdate struct {
	Channel, TS, Text string
	When              time.Time
}

// recordedPost captures one PostMessage call for assertion.
type recordedPost struct {
	ChannelID, ThreadTS, Text string
	// ReturnTS is the ts returned to the caller (set by fakePoster).
	ReturnTS string
}

// fakeUpdater records every UpdateMessage call. rateLimit fails the next
// `rateLimit` calls with a Slack rate-limit error before resuming success.
type fakeUpdater struct {
	mu        sync.Mutex
	updates   []recordedUpdate
	rateLimit int
}

func (f *fakeUpdater) UpdateMessage(_ context.Context, channel, ts, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rateLimit > 0 {
		f.rateLimit--
		// Match real slack-go error wording so the sink's classifier triggers.
		return errors.New("slack server error: rate_limited")
	}
	f.updates = append(f.updates, recordedUpdate{Channel: channel, TS: ts, Text: text, When: time.Now()})
	return nil
}

func (f *fakeUpdater) snapshot() []recordedUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedUpdate, len(f.updates))
	copy(out, f.updates)
	return out
}

// fakePoster records every PostMessage call and returns a fixed ts.
// rateLimit fails the next `rateLimit` calls with a rate-limit error.
type fakePoster struct {
	mu        sync.Mutex
	posts     []recordedPost
	returnTS  string // ts returned on every successful call
	rateLimit int
	postErr   error // sticky non-rate-limit error; set to trigger permanent failure
}

func (f *fakePoster) PostMessage(_ context.Context, channelID, threadTS, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rateLimit > 0 {
		f.rateLimit--
		return "", errors.New("slack server error: rate_limited")
	}
	if f.postErr != nil {
		return "", f.postErr
	}
	ts := f.returnTS
	if ts == "" {
		ts = "1614191050.000100"
	}
	f.posts = append(f.posts, recordedPost{ChannelID: channelID, ThreadTS: threadTS, Text: text, ReturnTS: ts})
	return ts, nil
}

func (f *fakePoster) snapshot() []recordedPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedPost, len(f.posts))
	copy(out, f.posts)
	return out
}

// sessWithBinding builds a SessionInfo with channel_id + thread_ts populated.
func sessWithBinding(channelID, threadTS string) channelkinds.SessionInfo {
	return channelkinds.SessionInfo{
		Namespace: "default",
		Name:      "sess-stream",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng",
			Kind: "slack",
			External: map[string]string{
				"channel_id": channelID,
				"thread_ts":  threadTS,
			},
		},
	}
}

func deltaEnv(t *testing.T, eventType, text, toolName string) channelevents.Envelope {
	t.Helper()
	pl, err := json.Marshal(channelevents.AssistantStreamDeltaPayload{
		EventType: eventType,
		Text:      text,
		ToolName:  toolName,
	})
	require.NoError(t, err, "marshal delta payload")
	return channelevents.Envelope{
		Version: 1,
		Kind:    channelevents.KindAssistantStreamDelta,
		Session: channelevents.SessionRef{Namespace: "default", Name: "sess-stream"},
		Payload: pl,
	}
}

// newTestSink constructs a StreamDeltaSink with the given updater and poster
// and a short debounce window for tests.
func newTestSink(upd *fakeUpdater, pst *fakePoster, debounce time.Duration) *StreamDeltaSink {
	return NewStreamDeltaSink(StreamDeltaSinkConfig{
		Updater:        upd,
		Poster:         pst,
		DebounceWindow: debounce,
	})
}

// TestStreamDeltaSink_FirstDeltaPostsThenUpdatesDebouncesTextDeltas verifies
// that the first flush calls Poster.PostMessage (creating the streaming bubble)
// and subsequent flushes call Updater.UpdateMessage on the same ts with the
// cumulative text.
func TestStreamDeltaSink_FirstDeltaPostsThenUpdatesDebouncesTextDeltas(t *testing.T) {
	upd := &fakeUpdater{}
	pst := &fakePoster{returnTS: "1614191050.000100"}
	sink := newTestSink(upd, pst, 50*time.Millisecond)
	defer sink.Close()

	sess := sessWithBinding("C01ABCDEF", "1614191050.000000")
	for _, frag := range []string{"hello ", "world ", "from ", "agent"} {
		require.NoError(t,
			sink.OnDelta(context.Background(), sess, deltaEnv(t, "text_delta", frag, "")),
			"OnDelta")
	}
	// First flush at ~50ms; allow generous slack.
	time.Sleep(150 * time.Millisecond)

	posts := pst.snapshot()
	require.Len(t, posts, 1, "exactly one PostMessage call expected (first flush)")
	assert.Equal(t, "C01ABCDEF", posts[0].ChannelID, "post channel")
	assert.Equal(t, "hello world from agent", posts[0].Text, "post text should be concatenation")

	// No UpdateMessage yet (first flush went through PostMessage path).
	assert.Empty(t, upd.snapshot(), "no UpdateMessage expected after first flush only")
}

// TestStreamDeltaSink_SecondFlushUpdatesStreamingBubble verifies that after
// the streaming bubble has been posted, subsequent flushes call UpdateMessage
// on the same ts.
func TestStreamDeltaSink_SecondFlushUpdatesStreamingBubble(t *testing.T) {
	upd := &fakeUpdater{}
	pst := &fakePoster{returnTS: "bubble.ts"}
	sink := newTestSink(upd, pst, 30*time.Millisecond)
	defer sink.Close()

	sess := sessWithBinding("C01ABCDEF", "1614191050.000000")
	require.NoError(t, sink.OnDelta(context.Background(), sess, deltaEnv(t, "text_delta", "first", "")))
	time.Sleep(80 * time.Millisecond) // first flush → PostMessage

	require.Len(t, pst.snapshot(), 1, "should have posted bubble after first flush")

	require.NoError(t, sink.OnDelta(context.Background(), sess, deltaEnv(t, "text_delta", " second", "")))
	time.Sleep(80 * time.Millisecond) // second flush → UpdateMessage

	updates := upd.snapshot()
	require.NotEmpty(t, updates, "UpdateMessage should have fired on second flush")
	last := updates[len(updates)-1]
	assert.Equal(t, "bubble.ts", last.TS, "update ts must match posted bubble ts")
	assert.Equal(t, "first second", last.Text, "cumulative text")
	assert.Equal(t, "C01ABCDEF", last.Channel, "update channel routing")
}

// TestStreamDeltaSink_StopFinalizesForConsumeFinalTarget verifies that a
// "stop" event moves the bubble ts to finalizedTS, and ConsumeFinalTarget
// returns it once then returns "" on the next call.
func TestStreamDeltaSink_StopFinalizesForConsumeFinalTarget(t *testing.T) {
	upd := &fakeUpdater{}
	pst := &fakePoster{returnTS: "finalized.ts"}
	sink := newTestSink(upd, pst, 30*time.Millisecond)
	defer sink.Close()

	sess := sessWithBinding("C01ABCDEF", "1614191050.000000")
	require.NoError(t, sink.OnDelta(context.Background(), sess, deltaEnv(t, "text_delta", "hello", "")))
	time.Sleep(80 * time.Millisecond) // flush → PostMessage

	require.Len(t, pst.snapshot(), 1, "bubble must be posted before stop")

	require.NoError(t, sink.OnDelta(context.Background(), sess, deltaEnv(t, "stop", "", "")))

	// ConsumeFinalTarget returns the bubble ts once, then clears it.
	got := sink.ConsumeFinalTarget("C01ABCDEF", "1614191050.000000")
	assert.Equal(t, "finalized.ts", got, "first ConsumeFinalTarget must return bubble ts")

	second := sink.ConsumeFinalTarget("C01ABCDEF", "1614191050.000000")
	assert.Empty(t, second, "second ConsumeFinalTarget must return empty (already consumed)")
}

// TestStreamDeltaSink_StopClearsBuffer verifies that a "stop" event resets
// the buffer so the next turn starts fresh.
func TestStreamDeltaSink_StopClearsBuffer(t *testing.T) {
	upd := &fakeUpdater{}
	pst := &fakePoster{returnTS: "1614191050.000100"}
	sink := newTestSink(upd, pst, 50*time.Millisecond)
	defer sink.Close()
	sess := sessWithBinding("C01ABCDEF", "1614191050.000000")

	send := func(eventType, text string) {
		t.Helper()
		require.NoError(t,
			sink.OnDelta(context.Background(), sess, deltaEnv(t, eventType, text, "")),
			"OnDelta")
	}
	send("text_delta", "hello")
	time.Sleep(120 * time.Millisecond) // first flush → PostMessage "hello"
	send("stop", "")
	// After stop, ConsumeFinalTarget clears the finalizedTS so the next turn
	// can post a fresh bubble. Simulate that.
	_ = sink.ConsumeFinalTarget("C01ABCDEF", "1614191050.000000")

	send("text_delta", "world")
	time.Sleep(120 * time.Millisecond) // second flush → PostMessage "world" (fresh bubble)

	posts := pst.snapshot()
	// First: "hello"; second: "world" (buffer was reset after stop).
	require.GreaterOrEqual(t, len(posts), 2, "expected at least 2 posts (one per turn)")
	assert.Equal(t, "hello", posts[0].Text, "first.Text")
	// The second post should NOT carry "hello" — buffer was reset.
	last := posts[len(posts)-1]
	assert.Equal(t, "world", last.Text, "last.Text (stop should clear buffer)")
}

// TestStreamDeltaSink_429DoublesDebounce verifies that a rate-limit error on
// the first PostMessage doubles the debounce window before retrying.
func TestStreamDeltaSink_429DoublesDebounce(t *testing.T) {
	upd := &fakeUpdater{}
	pst := &fakePoster{rateLimit: 1, returnTS: "1614191050.000100"}
	sink := newTestSink(upd, pst, 50*time.Millisecond)
	defer sink.Close()
	sess := sessWithBinding("C01ABCDEF", "1614191050.000000")

	send := func(text string) {
		t.Helper()
		require.NoError(t,
			sink.OnDelta(context.Background(), sess, deltaEnv(t, "text_delta", text, "")),
			"OnDelta")
	}

	send("a")
	// First flush attempt fires at ~50ms, fails 429, debounce doubles to 100ms.
	time.Sleep(80 * time.Millisecond)
	send("b")
	// Without 429-doubling, a flush would land at ~80+50=130ms total. With
	// doubling to 100ms, the retry-from-failure was scheduled at ~50ms with
	// 100ms window, so flush expected at ~150ms. We check at 100ms — should
	// still be empty.
	time.Sleep(20 * time.Millisecond)
	assert.Empty(t, pst.snapshot(),
		"expected zero successful posts yet (still in doubled-debounce window)")

	// Now wait long enough for the doubled-debounce flush to fire.
	time.Sleep(200 * time.Millisecond)
	posts := pst.snapshot()
	require.NotEmpty(t, posts, "expected flush to have fired by now after 429 backoff")
	// The successful post should carry the cumulative text.
	last := posts[len(posts)-1]
	assert.Equal(t, "ab", last.Text, "last.Text should be cumulative after 429 retry")
}

// TestStreamDeltaSink_ToolUseStartRendersMarker verifies that a tool_use_start
// event appends the "🔧 calling `<name>`" marker to the buffer.
func TestStreamDeltaSink_ToolUseStartRendersMarker(t *testing.T) {
	upd := &fakeUpdater{}
	pst := &fakePoster{returnTS: "1614191050.000100"}
	sink := newTestSink(upd, pst, 30*time.Millisecond)
	defer sink.Close()
	sess := sessWithBinding("C01ABCDEF", "1614191050.000000")

	require.NoError(t,
		sink.OnDelta(context.Background(), sess, deltaEnv(t, "text_delta", "thinking… ", "")),
		"OnDelta text_delta")
	require.NoError(t,
		sink.OnDelta(context.Background(), sess, deltaEnv(t, "tool_use_start", "", "code_gh")),
		"OnDelta tool_use_start")
	time.Sleep(120 * time.Millisecond)

	posts := pst.snapshot()
	require.NotEmpty(t, posts, "no posts fired")
	last := posts[len(posts)-1]
	assert.Contains(t, last.Text, "thinking… ", "missing pre-tool text")
	assert.Contains(t, last.Text, "code_gh", "missing tool name")
}

// TestStreamDeltaSink_NoPosterDropsSilently verifies that without a Poster
// the sink drops all deltas silently and ConsumeFinalTarget returns "".
func TestStreamDeltaSink_NoPosterDropsSilently(t *testing.T) {
	upd := &fakeUpdater{}
	sink := NewStreamDeltaSink(StreamDeltaSinkConfig{
		Updater:        upd,
		Poster:         nil, // no poster → no streaming surface
		DebounceWindow: 30 * time.Millisecond,
	})
	defer sink.Close()
	sess := sessWithBinding("C01ABCDEF", "1614191050.000000")

	require.NoError(t,
		sink.OnDelta(context.Background(), sess, deltaEnv(t, "text_delta", "hello", "")),
		"OnDelta")
	time.Sleep(80 * time.Millisecond)

	assert.Empty(t, upd.snapshot(), "expected zero updates without a poster")
	assert.Empty(t,
		sink.ConsumeFinalTarget("C01ABCDEF", "1614191050.000000"),
		"ConsumeFinalTarget must return empty when no bubble was posted")
}

// TestStreamDeltaSink_ForkChild_PostsTheBubbleUnderTheRoot pins that a fork
// child (a binding with no thread_ts yet) has its streaming bubble posted
// under the ThreadRoot-supplied link-back root, not at channel level. The
// sender's own fork-root guard resolves the SAME root and consults
// ConsumeFinalTarget keyed by it; if the sink instead keys the bubble under
// an empty thread_ts (as it did before ThreadRoot existed), that lookup
// misses and the sender posts a second, duplicate reply. With ThreadRoot nil
// the existing behavior (post at channel level) must be unchanged.
func TestStreamDeltaSink_ForkChild_PostsTheBubbleUnderTheRoot(t *testing.T) {
	t.Run("ThreadRoot configured: bubble posts under the returned root", func(t *testing.T) {
		upd := &fakeUpdater{}
		pst := &fakePoster{returnTS: "1614191050.000100"}
		var gotSess channelkinds.SessionInfo
		var gotChannelID string
		sink := NewStreamDeltaSink(StreamDeltaSinkConfig{
			Updater:        upd,
			Poster:         pst,
			DebounceWindow: 20 * time.Millisecond,
			ThreadRoot: func(_ context.Context, sess channelkinds.SessionInfo, channelID string) string {
				gotSess = sess
				gotChannelID = channelID
				return "root.1"
			},
		})
		defer sink.Close()

		sess := channelkinds.SessionInfo{
			Namespace: "default", Name: "fork-child",
			Channel: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-eng", Kind: "slack",
				External: map[string]string{
					"channel_id": "C01ABCDEF",
					// thread_ts intentionally absent — a fork child's binding has no
					// thread yet, which is exactly the condition ThreadRoot exists for.
				},
			},
		}
		require.NoError(t, sink.OnDelta(context.Background(), sess, deltaEnv(t, "text_delta", "hi there", "")))
		time.Sleep(100 * time.Millisecond)

		posts := pst.snapshot()
		require.Len(t, posts, 1, "one bubble post expected")
		assert.Equal(t, "root.1", posts[0].ThreadTS, "the bubble must post under the ThreadRoot-supplied root")
		assert.Equal(t, "C01ABCDEF", gotChannelID, "ThreadRoot must be asked with the resolved channel_id")
		assert.Equal(t, "fork-child", gotSess.Name, "ThreadRoot must be asked with the session it's resolving for")
	})

	t.Run("ThreadRoot nil: unchanged behavior, bubble posts at channel level", func(t *testing.T) {
		upd := &fakeUpdater{}
		pst := &fakePoster{returnTS: "1614191050.000200"}
		sink := newTestSink(upd, pst, 20*time.Millisecond) // ThreadRoot nil
		defer sink.Close()

		sess := channelkinds.SessionInfo{
			Namespace: "default", Name: "fork-child",
			Channel: &spiceboxv1alpha1.ChannelBinding{
				External: map[string]string{"channel_id": "C01ABCDEF"},
			},
		}
		require.NoError(t, sink.OnDelta(context.Background(), sess, deltaEnv(t, "text_delta", "hi there", "")))
		time.Sleep(100 * time.Millisecond)

		posts := pst.snapshot()
		require.Len(t, posts, 1, "one bubble post expected")
		assert.Empty(t, posts[0].ThreadTS, "with no ThreadRoot configured, the bubble posts at channel level")
	})
}
