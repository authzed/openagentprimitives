package sessionview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/web/webui/internal/wstest"
)

// liveFakeDeps implements Deps via per-behavior func fields for the websocket
// handler tests, mirroring artifactview's liveFakeDeps. The widget-related
// fields default to inert zero behaviors (see the methods below) since most
// live-handler tests don't exercise widget forwarding — only
// TestLive_ForwardsWidgetOfferEnvelope_WithMintedHostURL (widgets_test.go)
// sets them.
type liveFakeDeps struct {
	checkInteract   func(ctx context.Context, ns, name, subject string) (bool, error)
	operatorURL     string
	memoryToken     string
	nc              *nats.Conn
	trustedOrigin   func() string
	sandboxBaseURL  string
	signWidgetToken func(ns, sess, artifactID string) (string, error)
	logger          logr.Logger
}

func (f *liveFakeDeps) CheckInteract(ctx context.Context, ns, name, subject string) (bool, error) {
	return f.checkInteract(ctx, ns, name, subject)
}
func (f *liveFakeDeps) OperatorURL() string   { return f.operatorURL }
func (f *liveFakeDeps) MemoryToken() string   { return f.memoryToken }
func (f *liveFakeDeps) NATS() *nats.Conn      { return f.nc }
func (f *liveFakeDeps) TrustedOrigin() string { return f.trustedOrigin() }
func (f *liveFakeDeps) Logger() logr.Logger   { return f.logger }

func (f *liveFakeDeps) SandboxBaseURL() string { return f.sandboxBaseURL }
func (f *liveFakeDeps) ActiveWidgets(ctx context.Context, ns, sess string) ([]WidgetRef, error) {
	return nil, nil
}
func (f *liveFakeDeps) SignWidgetToken(ns, sess, artifactID string) (string, error) {
	if f.signWidgetToken == nil {
		return "", assert.AnError
	}
	return f.signWidgetToken(ns, sess, artifactID)
}
func (f *liveFakeDeps) VerifyWidgetToken(token string) (string, string, string, error) {
	return "", "", "", assert.AnError
}
func (f *liveFakeDeps) FetchWidget(ctx context.Context, ns, sess, artifactID string) ([]byte, WidgetMeta, error) {
	return nil, WidgetMeta{}, assert.AnError
}

// liveTestDeps returns a happy-path Deps for the websocket handler.
// trustedOrigin is read through a closure so the test can set it to the test
// server's URL AFTER the server starts (the CheckOrigin gate compares against
// it) — mirrors artifactview's liveTestAV.
func liveTestDeps(trustedOrigin *string) *liveFakeDeps {
	return &liveFakeDeps{
		checkInteract: func(ctx context.Context, ns, name, subject string) (bool, error) { return true, nil },
		trustedOrigin: func() string { return *trustedOrigin },
		logger:        logr.Discard(),
	}
}

// dialLive mounts liveHandler(d) on the real route pattern (so
// r.PathValue("ns")/("name") populate exactly as production's http.ServeMux
// would), points trustedOrigin at the started server, and dials the ws
// endpoint with the trusted Origin header. conn is nil when the handshake is
// refused (e.g. CheckInteract denies) — resp still carries the HTTP status.
func dialLive(t *testing.T, d Deps, trustedOrigin *string, ns, name string) (*websocket.Conn, *http.Response) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/session-view/{ns}/{name}/live", liveHandler(d))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	*trustedOrigin = srv.URL

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/session-view/" + ns + "/" + name + "/live"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{srv.URL}})
	if conn != nil {
		t.Cleanup(func() { conn.Close() })
	}
	if err != nil {
		return nil, resp
	}
	return conn, resp
}

func readLive(t *testing.T, conn *websocket.Conn) liveMessage {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(wstest.Deadline(2*time.Second)))
	_, raw, err := conn.ReadMessage()
	require.NoError(t, err, "read ws frame")
	var msg liveMessage
	require.NoError(t, json.Unmarshal(raw, &msg), "unmarshal ws frame")
	return msg
}

func TestLive_Denied_HandshakeFailsWith403(t *testing.T) {
	var trusted string
	d := liveTestDeps(&trusted)
	d.checkInteract = func(ctx context.Context, ns, name, subject string) (bool, error) { return false, nil }

	conn, resp := dialLive(t, d, &trusted, "ns1", "sess1")
	assert.Nil(t, conn, "denied request must not complete the ws handshake")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestLive_CheckInteractError_HandshakeFailsWith500(t *testing.T) {
	var trusted string
	d := liveTestDeps(&trusted)
	d.checkInteract = func(ctx context.Context, ns, name, subject string) (bool, error) {
		return false, assert.AnError
	}

	conn, resp := dialLive(t, d, &trusted, "ns1", "sess1")
	assert.Nil(t, conn, "an authz error must not complete the ws handshake")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

// TestLive_ReplaysHistoryThenForwardsPublishedEnvelope covers the brief's core
// scenario: connect → the first frame replays the session's durable
// transcript (via a fake operator memory server), and a user_message envelope
// published afterward on the session's real NATS out.user_message subject
// arrives as a live "event" frame.
func TestLive_ReplaysHistoryThenForwardsPublishedEnvelope(t *testing.T) {
	const ns, name = "default", "sess1"
	at := time.Now().UTC().Truncate(time.Second)
	entries := []memory.Entry{
		turnTestEntry(t, ns, name, 0, "user", "hello", at),
		turnTestEntry(t, ns, name, 1, "assistant", "hi there", at.Add(time.Second)),
	}
	memSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer memSrv.Close()

	natsSrv := natstest.RunServer(&natsserver.Options{Port: -1})
	defer natsSrv.Shutdown()
	subConn, err := nats.Connect(natsSrv.ClientURL())
	require.NoError(t, err)
	defer subConn.Close()
	pubConn, err := nats.Connect(natsSrv.ClientURL())
	require.NoError(t, err)
	defer pubConn.Close()

	var trusted string
	d := liveTestDeps(&trusted)
	d.operatorURL = memSrv.URL
	d.memoryToken = "webd-token"
	d.nc = subConn

	conn, resp := dialLive(t, d, &trusted, ns, name)
	require.NotNil(t, conn)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	snap := readLive(t, conn)
	require.Equal(t, "snapshot", snap.Type)
	require.Len(t, snap.History, 2, "the snapshot replays the durable transcript")
	assert.Equal(t, "user", snap.History[0].Role)
	assert.Equal(t, "hello", snap.History[0].Text)
	assert.Equal(t, "agent", snap.History[1].Role)
	assert.Equal(t, "hi there", snap.History[1].Text)

	evt := publishUntilDelivered(t, conn, pubConn, ns, name,
		channelevents.KindUserMessage, channelevents.OutboundUserMessagePayload{Text: "live update"})
	require.Equal(t, "event", evt.Type)
	require.NotNil(t, evt.Event)
	assert.Equal(t, channelevents.KindUserMessage, evt.Event.Kind)
	var pl channelevents.OutboundUserMessagePayload
	require.NoError(t, json.Unmarshal(evt.Event.Payload, &pl))
	assert.Equal(t, "live update", pl.Text)
}

// TestLive_NoMemoryAccess_SendsEmptySnapshotThenStillForwardsLive covers the
// degrade path: no OperatorURL/MemoryToken configured (ReadHistory returns
// ErrNoMemoryAccess) still yields a snapshot frame (empty), and live updates
// still flow — the socket never dies over a history-replay failure.
func TestLive_NoMemoryAccess_SendsEmptySnapshotThenStillForwardsLive(t *testing.T) {
	const ns, name = "default", "sess1"
	natsSrv := natstest.RunServer(&natsserver.Options{Port: -1})
	defer natsSrv.Shutdown()
	subConn, err := nats.Connect(natsSrv.ClientURL())
	require.NoError(t, err)
	defer subConn.Close()
	pubConn, err := nats.Connect(natsSrv.ClientURL())
	require.NoError(t, err)
	defer pubConn.Close()

	var trusted string
	d := liveTestDeps(&trusted)
	d.nc = subConn

	conn, resp := dialLive(t, d, &trusted, ns, name)
	require.NotNil(t, conn)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	snap := readLive(t, conn)
	require.Equal(t, "snapshot", snap.Type)
	assert.Empty(t, snap.History, "no memory access degrades to an empty snapshot, not a dead socket")

	evt := publishUntilDelivered(t, conn, pubConn, ns, name,
		channelevents.KindUserMessage, channelevents.OutboundUserMessagePayload{Text: "still live"})
	require.Equal(t, "event", evt.Type)
	require.NotNil(t, evt.Event)
}

// TestLive_NilNATS_SnapshotOnlyNoPanic covers the other degrade path: NATS not
// configured at all. The socket still opens and delivers the snapshot; there
// is simply nothing further to read (no panic on a nil subscriber).
func TestLive_NilNATS_SnapshotOnlyNoPanic(t *testing.T) {
	const ns, name = "default", "sess1"
	var trusted string
	d := liveTestDeps(&trusted) // d.nc left nil

	conn, resp := dialLive(t, d, &trusted, ns, name)
	require.NotNil(t, conn)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	snap := readLive(t, conn)
	assert.Equal(t, "snapshot", snap.Type)
}

// TestLive_OversizeClientFrame_ClosesTheSocket pins the read bound. The
// session-view socket is push-only — the browser sends only close/pong control
// frames, never data — so a client streaming an unbounded data frame is either
// broken or hostile and must not be able to grow the server's read buffer
// without limit. Mirrors artifactview's test of the same name; webd is a single
// shared pod, so an uncapped read here is an OOM any subject holding
// agentsession#interact can trigger.
func TestLive_OversizeClientFrame_ClosesTheSocket(t *testing.T) {
	const ns, name = "default", "sess1"
	var trusted string
	d := liveTestDeps(&trusted) // d.nc left nil: the snapshot alone proves the socket is live

	conn, resp := dialLive(t, d, &trusted, ns, name)
	require.NotNil(t, conn)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	snap := readLive(t, conn)
	require.Equal(t, "snapshot", snap.Type, "the socket is live before the oversize write")

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, make([]byte, maxLiveClientFrame+1)))

	require.NoError(t, conn.SetReadDeadline(wstest.Deadline(2*time.Second)))
	_, _, err := conn.ReadMessage()
	require.Error(t, err, "a frame over the read limit must terminate the socket")
	assert.True(t, websocket.IsCloseError(err, websocket.CloseMessageTooBig),
		"the server must close with 1009 message-too-big, got %v", err)
}

// publishUntilDelivered publishes an out.<kind> envelope repeatedly (on a
// short interval) until the resulting "event" frame arrives on a SINGLE
// background read loop, absorbing the inherent race between the server's
// background WatchOutbound subscription registering and the test's publish —
// core NATS has no queueing for a not-yet-registered subscriber, so a publish
// that lands before the subscription is up is silently dropped; re-publishing
// is the standard way to make that race deterministic in a test (same shape
// as livemirror/watch_test.go's Flush-before-publish).
//
// The read side deliberately does NOT retry conn.ReadMessage after a timeout:
// per gorilla/websocket's contract, once ReadMessage returns ANY error the
// connection is PERMANENTLY marked failed and every subsequent call returns
// the same cached error without blocking — a "retry on timeout" loop that
// calls ReadMessage again after its own timeout doesn't actually wait for
// anything past the first attempt, and spins until gorilla's own
// repeated-read panic guard fires. So there is exactly one blocking read loop
// here, on a single generous deadline, in its own goroutine; the outer loop
// only re-publishes.
func publishUntilDelivered(t *testing.T, conn *websocket.Conn, pubConn *nats.Conn, ns, name string, kind channelevents.Kind, payload any) liveMessage {
	t.Helper()
	// Scaled, not a const: this is the window a frame that IS coming has to
	// arrive in, and it bounds the republish loop below as well as the read.
	overallTimeout := wstest.Scale(5 * time.Second)
	publish := func(subj string, data []byte) error { return pubConn.Publish(subj, data) }

	msgCh := make(chan liveMessage, 1)
	errCh := make(chan error, 1)
	go func() {
		if err := conn.SetReadDeadline(time.Now().Add(overallTimeout)); err != nil {
			errCh <- err
			return
		}
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			var msg liveMessage
			if json.Unmarshal(raw, &msg) == nil && msg.Type == "event" {
				msgCh <- msg
				return
			}
			// Not the frame we're waiting for (e.g. a stray status/message
			// frame) — keep reading on the same deadline.
		}
	}()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.Now().Add(overallTimeout)
	require.NoError(t, channelevents.PublishOut(publish, ns, name, kind, payload))
	require.NoError(t, pubConn.Flush())
	for {
		select {
		case msg := <-msgCh:
			return msg
		case err := <-errCh:
			t.Fatalf("no live event frame received before deadline: %v", err)
			return liveMessage{}
		case <-ticker.C:
			if time.Now().After(deadline) {
				t.Fatal("no live event frame received before deadline")
				return liveMessage{}
			}
			require.NoError(t, channelevents.PublishOut(publish, ns, name, kind, payload))
			require.NoError(t, pubConn.Flush())
		}
	}
}

// turnTestEntry builds a memory Entry in the on-wire shape turn.EntryToTurn
// decodes — mirrors pkg/web/webui/chat/transcript_test.go's helper of the same
// shape (kept package-local: a small static test fixture builder, not shared
// production logic).
func turnTestEntry(t *testing.T, ns, name string, idx int, role, text string, at time.Time) memory.Entry {
	t.Helper()
	content, err := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	require.NoError(t, err)
	return memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: ns + "/" + name},
		Kind:      turn.KindName,
		ID:        turn.EntryID(idx, role),
		CreatedAt: at,
		Content:   content,
	}
}
