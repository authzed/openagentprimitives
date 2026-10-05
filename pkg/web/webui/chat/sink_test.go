package chat

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
)

// newWSConnPair upgrades a real websocket over a throwaway httptest server and
// returns the server-side and client-side *websocket.Conn. The server handler
// upgrades and hands the conn back to the test; after the ws hijack, returning
// from the handler does NOT close the hijacked conn, so both ends stay usable
// until cleanup. This gives the wsSink concurrency tests a genuine
// *websocket.Conn to exercise (there is no exported way to fabricate one).
func newWSConnPair(t *testing.T) (server, client *websocket.Conn) {
	t.Helper()
	serverCh := make(chan *websocket.Conn, 1)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverCh <- c
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err, "dial test websocket")
	t.Cleanup(func() { _ = client.Close() })

	select {
	case server = <-serverCh:
	case <-time.After(2 * time.Second):
		t.Fatal("server never upgraded the test websocket")
	}
	t.Cleanup(func() { _ = server.Close() })
	return server, client
}

func sinkIsDead(s *wsSink) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead
}

// TestWSSink_WriteFailure_MarksDeadStopsWriterAndNoOps covers the audit's
// untested wsSink failure path: when a conn write fails, the writer goroutine
// must mark the sink dead, close its frame channel (so it exits — no leak),
// close the connection, and make every further Emit a safe no-op.
func TestWSSink_WriteFailure_MarksDeadStopsWriterAndNoOps(t *testing.T) {
	server, _ := newWSConnPair(t)
	ref := browser.SessionRef{Namespace: newChatSessionNamespace, Name: "sess-broken"}
	sink := newWSSink(server, logr.Discard(), ref)

	// Break the transport out from under the sink so the writer goroutine's
	// next write fails deterministically (no reliance on TCP send-buffer fill).
	require.NoError(t, server.UnderlyingConn().Close())

	// One Emit is enough: the writer picks the frame up, the write fails, and
	// the failure path fires.
	sink.Emit(browser.MsgUserMessage{Session: ref, Text: "will fail"})

	require.Eventually(t, func() bool { return sinkIsDead(sink) }, 2*time.Second, 5*time.Millisecond,
		"a failed write must mark the sink dead")

	// The writer goroutine has stopped: it closed (and drained) its frames
	// channel, so a receive returns the closed-channel zero value.
	select {
	case _, ok := <-sink.frames:
		assert.False(t, ok, "the writer must have closed its frames channel on the failed write")
	case <-time.After(time.Second):
		t.Fatal("frames channel was neither closed nor drained — the writer goroutine may have leaked")
	}

	// Further Emits are safe no-ops: no panic, and nothing is buffered onto the
	// dead sink.
	assert.NotPanics(t, func() {
		sink.Emit(browser.MsgUserMessage{Session: ref, Text: "ignored"})
		sink.Emit(browser.MsgNotification{Session: ref, Text: "also ignored"})
	}, "Emit on a dead sink must be a safe no-op")
	assert.Equal(t, 0, len(sink.frames), "a dead sink must not buffer further frames")

	// Close is idempotent on an already-dead sink.
	assert.NotPanics(t, sink.Close, "Close on an already-dead sink must be a no-op")
}

// TestSessionEntryEmit_DoesNotBlockOnFullSink proves the core non-blocking
// guarantee: with one sink's buffer already full and nothing draining it,
// sessionEntry.Emit must still return promptly (drop-oldest), never stalling
// the shared outbound goroutine on a back-pressured tab. The sink is built by
// hand (no writer goroutine) specifically so the buffer stays pinned full for
// the duration of the test.
func TestSessionEntryEmit_DoesNotBlockOnFullSink(t *testing.T) {
	server, _ := newWSConnPair(t)
	ref := browser.SessionRef{Namespace: newChatSessionNamespace, Name: "sess-full"}
	sink := &wsSink{
		conn:   server,
		logger: logr.Discard(),
		ref:    ref,
		frames: make(chan []byte, wsSinkBufferSize),
	}
	for i := 0; i < wsSinkBufferSize; i++ {
		sink.emitFrame([]byte("x"))
	}
	require.Equal(t, wsSinkBufferSize, len(sink.frames), "precondition: the sink buffer is full")

	entry := newSessionEntry(newChatSessionNamespace, "sess-full", "user:owner")
	require.True(t, entry.attach(sink))

	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			entry.Emit(browser.MsgUserMessage{Session: ref, Text: "over-capacity"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sessionEntry.Emit blocked on a full sink buffer — the outbound path must never block on a slow tab")
	}

	// drop-oldest keeps the buffer pinned at capacity: it never grows past it
	// and never blocks waiting for room.
	assert.Equal(t, wsSinkBufferSize, len(sink.frames),
		"drop-oldest must hold the buffer at capacity, never grow it")
}

// TestToFrame_MapsRenderEventsToWireFrames pins the browser.Msg* → wsFrame
// discriminator mapping the browser chat UI dispatches on. It covers the
// events the live surfaces depend on — the finished reply/notification/plan
// plus the three live ones (token stream deltas, turn progress, turn
// activity) — so a renamed frame type breaks a Go test rather than silently
// dropping to the UI's ignore-fallback.
func TestToFrame_MapsRenderEventsToWireFrames(t *testing.T) {
	ref := browser.SessionRef{Namespace: "default", Name: "s1"}
	cases := []struct {
		name     string
		msg      any
		wantType string
	}{
		{"session_opening", browser.MsgSessionOpening{Session: ref, Opening: &channelevents.SessionOpening{Summary: "Async task", Instructions: "Exact prompt"}}, "session_opening"},
		{"user_message", browser.MsgUserMessage{Session: ref, Text: "hi"}, "user_message"},
		{"user_echo", browser.MsgUserEcho{Session: ref, Text: "from the artifact view", Author: "a@example.com", RequestID: "req-9"}, "user_echo"},
		{"notification", browser.MsgNotification{Session: ref, Text: "working"}, "notification"},
		{"plan_update", browser.MsgPlanUpdate{Session: ref}, "plan_update"},
		{
			"stream_delta",
			browser.MsgStreamDelta{Session: ref, Payload: channelevents.AssistantStreamDeltaPayload{EventType: "text_delta", Text: "he"}},
			"stream_delta",
		},
		{
			"turn_progress",
			browser.MsgTurnProgress{Session: ref, Payload: channelevents.TurnProgressPayload{InputTokens: 1200, OutputTokens: 6400, ElapsedSeconds: 34}},
			"turn_progress",
		},
		{
			"turn_activity",
			browser.MsgTurnActivity{Session: ref, Active: false, Cause: "awaiting_user_message"},
			"turn_activity",
		},
		{
			"operation_activity",
			browser.MsgOperationActivity{
				Session: ref,
				Payload: channelevents.OperationActivityPayload{
					Operations:  []channelevents.OperationActivityNode{{ID: "op-1", Description: "fetch goals", Active: true}},
					CompactLine: "fetch goals ‣ querying Linear",
				},
			},
			"operation_activity",
		},
		{
			"interrupt_applied",
			browser.MsgInterruptApplied{Session: ref, RequestID: "req-1", Outcome: "interrupted"},
			"interrupt_applied",
		},
		{
			"interaction_request",
			browser.MsgInteractionRequest{
				Session: ref,
				Payload: channelevents.InteractionRequestPayload{Category: "credential_link", RequestRef: "req-1", Lead: "Connect your account"},
			},
			"interaction_request",
		},
		{
			"interaction_applied",
			browser.MsgInteractionApplied{
				Session: ref,
				Payload: channelevents.InteractionAppliedPayload{Category: "credential_link", RequestRef: "req-1", Outcome: channelevents.OutcomeResolved},
			},
			"interaction_applied",
		},
		{
			"session_startup",
			sessionStartupMsg{Session: ref, Text: "Starting safety checks…", StillTrying: true},
			"session_startup",
		},
		{"session_ended", sessionEndedMsg{Session: ref, Reason: "succeeded"}, "session_ended"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame, ok := toFrame(tc.msg)
			require.True(t, ok, "toFrame must map %T", tc.msg)
			assert.Equal(t, tc.wantType, frame.Type)
			assert.Equal(t, ref, frame.Session)
			assert.NotNil(t, frame.Payload)
		})
	}
}

func TestToFrame_UnknownTypeDropped(t *testing.T) {
	_, ok := toFrame(struct{ X int }{X: 1})
	assert.False(t, ok, "an unrecognized message type must not map to a frame")
}
