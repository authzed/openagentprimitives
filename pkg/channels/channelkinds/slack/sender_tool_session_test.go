package slack

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// recordingClient is a slackClient stub recording post/update calls. The
// flush timer callback issues UpdateMessageContext from its own goroutine,
// so every counter access is mutex-guarded.
type recordingClient struct {
	mu      sync.Mutex
	posts   int
	updates int
	lastTS  string
}

func (c *recordingClient) PostMessageContext(_ context.Context, ch string, _ ...slackapi.MsgOption) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.posts++
	c.lastTS = "ts-1"
	return ch, "ts-1", nil
}

func (c *recordingClient) UpdateMessageContext(_ context.Context, _, ts string, _ ...slackapi.MsgOption) (string, string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates++
	return "", ts, "", nil
}

func (c *recordingClient) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.posts, c.updates
}

// The remaining slackClient methods are never exercised by the
// tool_session tests — they panic so an accidental call is loud.
func (c *recordingClient) PostEphemeralContext(context.Context, string, string, ...slackapi.MsgOption) (string, error) {
	panic("recordingClient: unused")
}

func (c *recordingClient) OpenConversationContext(context.Context, *slackapi.OpenConversationParameters) (*slackapi.Channel, bool, bool, error) {
	panic("recordingClient: unused")
}

func (c *recordingClient) SetAssistantThreadsStatusContext(context.Context, slackapi.AssistantThreadsSetStatusParameters) error {
	panic("recordingClient: unused")
}

func (c *recordingClient) SetAssistantThreadsTitleContext(context.Context, slackapi.AssistantThreadsSetTitleParameters) error {
	panic("recordingClient: unused")
}

func (c *recordingClient) UploadFileContext(context.Context, slackapi.UploadFileParameters) (*slackapi.FileSummary, error) {
	panic("recordingClient: unused")
}

func (c *recordingClient) GetUserByEmailContext(context.Context, string) (*slackapi.User, error) {
	panic("recordingClient: unused")
}

func (c *recordingClient) GetUsersContext(context.Context, ...slackapi.GetUsersOption) ([]slackapi.User, error) {
	panic("recordingClient: unused")
}

func (c *recordingClient) OpenViewContext(context.Context, string, slackapi.ModalViewRequest) (*slackapi.ViewResponse, error) {
	panic("recordingClient: unused")
}

func (c *recordingClient) GetPermalinkContext(context.Context, *slackapi.PermalinkParameters) (string, error) {
	return "", nil
}

func newTestToolSessionSender(cli slackClient) *toolSessionSender {
	return &toolSessionSender{
		cli:           cli,
		refs:          newToolSessionRefCache(),
		debounceWin:   20 * time.Millisecond,
		fallbackEvery: 60 * time.Millisecond,
	}
}

func testSession() channelkinds.SessionInfo {
	return channelkinds.SessionInfo{
		Channel: &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C1"}},
	}
}

func eventEnvelope(t *testing.T, pl channelevents.ToolSessionEventPayload) channelevents.Envelope {
	t.Helper()
	b, err := json.Marshal(pl)
	require.NoError(t, err)
	return channelevents.Envelope{Kind: channelevents.KindToolSessionEvent, Payload: b}
}

func TestToolSession_FirstEventPosts_BurstCoalesces(t *testing.T) {
	cli := &recordingClient{}
	s := newTestToolSessionSender(cli)
	ctx := context.Background()
	sess := testSession()

	_, err := s.Send(ctx, sess, eventEnvelope(t, channelevents.ToolSessionEventPayload{
		ToolCallRef: "tc-1", EventType: "tool_use_start", ToolName: "Read", Reason: "do the thing"}))
	require.NoError(t, err)
	posts, _ := cli.counts()
	assert.Equal(t, 1, posts, "first event posts")

	for i := 0; i < 5; i++ {
		_, err := s.Send(ctx, sess, eventEnvelope(t, channelevents.ToolSessionEventPayload{
			ToolCallRef: "tc-1", EventType: "text_delta", Text: "x"}))
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool {
		_, u := cli.counts()
		return u >= 1
	}, time.Second, 5*time.Millisecond, "debounced update fires")
	// The 5-event burst within the 20ms debounce window coalesces into
	// exactly one flush — not one update per event. Read immediately
	// after Eventually so the ~60ms fallback tick hasn't fired yet.
	_, updates := cli.counts()
	assert.Equal(t, 1, updates, "burst coalesced into one debounced update")

	s.refs.get("tc-1").stop() // stop the test goroutine's timer
}

func TestToolSession_ResultFinalizesAndDrops(t *testing.T) {
	cli := &recordingClient{}
	s := newTestToolSessionSender(cli)
	ctx := context.Background()
	sess := testSession()

	_, err := s.Send(ctx, sess, eventEnvelope(t, channelevents.ToolSessionEventPayload{
		ToolCallRef: "tc-2", EventType: "tool_use_start", ToolName: "claude"}))
	require.NoError(t, err)
	_, err = s.Send(ctx, sess, eventEnvelope(t, channelevents.ToolSessionEventPayload{
		ToolCallRef: "tc-2", EventType: "result", OK: true, CostUSD: 0.01, DurationMs: 1234}))
	require.NoError(t, err)

	_, updates := cli.counts()
	assert.GreaterOrEqual(t, updates, 1, "result triggers a final update")
	s.refs.mu.Lock()
	_, present := s.refs.m["tc-2"]
	s.refs.mu.Unlock()
	assert.False(t, present, "ref dropped after result")
}

func TestToolSession_FallbackTickAdvancesTimer(t *testing.T) {
	cli := &recordingClient{}
	s := newTestToolSessionSender(cli)
	ctx := context.Background()

	_, err := s.Send(ctx, testSession(), eventEnvelope(t, channelevents.ToolSessionEventPayload{
		ToolCallRef: "tc-3", EventType: "tool_use_start", ToolName: "claude"}))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, u := cli.counts()
		return u >= 2
	}, time.Second, 10*time.Millisecond, "fallback ticks update the message while idle")

	s.refs.get("tc-3").stop()
}

func TestToolSession_RawDeltaPathRenders(t *testing.T) {
	cli := &recordingClient{}
	s := newTestToolSessionSender(cli)
	pl := channelevents.ToolSessionDeltaPayload{ToolCallRef: "tc-4", Stream: "stdout", Data: []byte("raw output\n")}
	b, err := json.Marshal(pl)
	require.NoError(t, err)
	_, err = s.Send(context.Background(), testSession(),
		channelevents.Envelope{Kind: channelevents.KindToolSessionDelta, Payload: b})
	require.NoError(t, err)
	posts, _ := cli.counts()
	assert.Equal(t, 1, posts, "raw delta path posts the Block Kit message too")

	tp := channelevents.ToolSessionDeltaPayload{ToolCallRef: "tc-4", Terminal: true, ExitReason: "completed", ExitCode: 0}
	tb, err := json.Marshal(tp)
	require.NoError(t, err)
	_, err = s.Send(context.Background(), testSession(),
		channelevents.Envelope{Kind: channelevents.KindToolSessionDelta, Payload: tb})
	require.NoError(t, err)
	s.refs.mu.Lock()
	_, present := s.refs.m["tc-4"]
	s.refs.mu.Unlock()
	assert.False(t, present, "ref dropped after terminal delta")
}

func TestToolSession_RedundantTerminalDeltaIgnored(t *testing.T) {
	cli := &recordingClient{}
	s := newTestToolSessionSender(cli)
	// A terminal delta for a ToolCallRef with no live message — e.g. the
	// OnTerminal raw delta trailing an already-finalized `result` event
	// — must not post a spurious empty "(no output yet)" message.
	tp := channelevents.ToolSessionDeltaPayload{ToolCallRef: "tc-gone", Terminal: true, ExitReason: "completed"}
	b, err := json.Marshal(tp)
	require.NoError(t, err)
	_, err = s.Send(context.Background(), testSession(),
		channelevents.Envelope{Kind: channelevents.KindToolSessionDelta, Payload: b})
	require.NoError(t, err)
	posts, updates := cli.counts()
	assert.Equal(t, 0, posts, "no message posted for a redundant terminal delta")
	assert.Equal(t, 0, updates, "no update either")
}

func TestToolSessionSeen(t *testing.T) {
	s := newToolSessionSeen()
	assert.False(t, s.saw("ns/a"), "unmarked session")
	s.mark("ns/a")
	assert.True(t, s.saw("ns/a"), "marked session")
	assert.False(t, s.saw("ns/b"), "a different session stays unmarked")
}

// TestToolSessionSender_ForkChild_ReportsTheThreadRootNotTheBubble pins the
// write-back contract for a restart-fork child whose tool bubble is the
// first sender to reach the channel. forkRootCache.ensure posts the
// link-back framing message and that message becomes the thread root; the
// bubble is then posted UNDER it, so the bubble's own ts is a nested reply,
// not the root. The relay patches the session's channel binding +
// LabelChannelKey from the returned thread_ts, and a user's later reply in
// that thread carries the ROOT ts — so reporting anything else here silently
// detaches the user's reply from the session.
func TestToolSessionSender_ForkChild_ReportsTheThreadRootNotTheBubble(t *testing.T) {
	const rootTS = "900.1"
	const backlinkTS = "900.2" // posted into the OLD thread; never read back by this test
	const bubbleTS = "900.3"
	cli := &fakeSlackClient{postedTSs: []string{rootTS, backlinkTS, bubbleTS}}
	k8s := fake.NewClientBuilder().WithScheme(forkRootScheme(t)).WithObjects(
		&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "child",
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationForkedFromThread: "T1:C1:1783536805.645439",
			},
		}},
	).Build()
	s := &toolSessionSender{
		deps:          channelkinds.Deps{K8sClient: k8s},
		cli:           cli,
		refs:          newToolSessionRefCache(),
		debounceWin:   20 * time.Millisecond,
		fallbackEvery: 60 * time.Millisecond,
		forkRoot:      newForkRootCache(),
	}
	sess := channelkinds.SessionInfo{
		Namespace: "ns", Name: "child",
		Channel: &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C1"}},
		// thread_ts intentionally absent: a fork child's first send arrives
		// with no thread yet — that's what triggers forkRoot.ensure below.
	}

	res, err := s.Send(context.Background(), sess, eventEnvelope(t, channelevents.ToolSessionEventPayload{
		ToolCallRef: "tc-fork", EventType: "tool_use_start", ToolName: "Read", Reason: "do the thing"}))
	require.NoError(t, err)

	require.Len(t, cli.postMessageCalls, 3,
		"postMessage[0]=root framing, postMessage[1]=parent-thread backlink, postMessage[2]=the tool bubble")

	assert.Equal(t, rootTS, res.External["thread_ts"],
		"write-back must report the THREAD ROOT so the relay patches the session binding to it")
	assert.NotEqual(t, bubbleTS, res.External["thread_ts"],
		"reporting the bubble's own ts would detach a user's later thread reply from the session")

	bubble := cli.postMessageCalls[2]
	assert.Equal(t, "C1", bubble.channelID, "the bubble posts into the same channel as the root")
	_, bubbleVals, err := slackapi.UnsafeApplyMsgOptions("test-token", bubble.channelID, "http://test.invalid/", bubble.options...)
	require.NoError(t, err, "UnsafeApplyMsgOptions(bubble)")
	assert.Equal(t, rootTS, bubbleVals.Get("thread_ts"), "the bubble itself was posted INTO the root thread")

	s.refs.get("tc-fork").stop() // stop the test goroutine's timer
}

// TestToolSessionSender_OrdinarySession_ReportsTheBubbleAsRoot pins the
// unchanged behavior for a non-fork session: with no enclosing thread, the
// bubble's own chat.postMessage IS the root, and forkRoot is left nil here —
// mirroring how Kind leaves it unset in direct-construction tests (treated
// as no fork children).
func TestToolSessionSender_OrdinarySession_ReportsTheBubbleAsRoot(t *testing.T) {
	const bubbleTS = "500.1"
	cli := &fakeSlackClient{postedTS: bubbleTS}
	s := &toolSessionSender{
		cli:           cli,
		refs:          newToolSessionRefCache(),
		debounceWin:   20 * time.Millisecond,
		fallbackEvery: 60 * time.Millisecond,
	}

	res, err := s.Send(context.Background(), testSession(), eventEnvelope(t, channelevents.ToolSessionEventPayload{
		ToolCallRef: "tc-plain", EventType: "tool_use_start", ToolName: "Read", Reason: "do the thing"}))
	require.NoError(t, err)

	require.Len(t, cli.postMessageCalls, 1, "no fork framing: only the bubble itself posts")
	assert.Equal(t, bubbleTS, res.External["thread_ts"], "with no enclosing thread the bubble itself IS the root")

	s.refs.get("tc-plain").stop() // stop the test goroutine's timer
}

func (c *recordingClient) GetUserInfoContext(context.Context, string, ...slackapi.GetUserInfoOption) (*slackapi.User, error) {
	return nil, nil
}
