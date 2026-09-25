package agentui

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui/internal/wstest"
)

// gorilla's default read limit is UNLIMITED, and this socket's read pump calls
// ReadMessage — which is NextReader plus io.ReadAll — so without a limit a
// single oversized frame is buffered whole before the JSON is even looked at.
// One shared webd pod, and the precondition is agentsession#interact on any
// one session, which every dashboard starter holds.
//
// Both sibling live sockets (sessionview, artifactview) set the same 1 KiB
// limit and one of them documents this exact hazard, so the omission here was
// a miss rather than a choice. The socket is push-only: the only frame a
// client may send is a small presence ping.
func TestLiveSocketBoundsAClientFrame(t *testing.T) {
	f := newLiveFixture(t, "user:alice")
	conn := f.dial(t)

	// Comfortably past the limit, far short of what an attacker would send.
	oversized := `{"type":"presence","pad":"` + strings.Repeat("A", 64*1024) + `"}`
	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(3*time.Second)))
	// The write itself may succeed — the server closes on READ — so the
	// failure to assert on is the server hanging up, not the write erroring.
	_ = conn.WriteMessage(websocket.TextMessage, []byte(oversized))

	// Drain until the socket errors: the mirror may legitimately push a frame
	// before the close lands, and that push is not the thing under test.
	require.NoError(t, conn.SetReadDeadline(wstest.Deadline(3*time.Second)))
	var err error
	for i := 0; i < 16; i++ {
		if _, _, err = conn.ReadMessage(); err != nil {
			break
		}
	}
	require.Error(t, err, "the server must hang up on an oversized frame rather than buffering it")
	assert.True(t,
		websocket.IsCloseError(err, websocket.CloseMessageTooBig) || websocket.IsUnexpectedCloseError(err) || strings.Contains(err.Error(), "close"),
		"expected a close after the read limit was exceeded, got %v", err)
}

// The limit must not break the one frame this socket legitimately accepts: a
// presence ping keeps the connection alive and must still be read.
func TestLiveSocketStillAcceptsAPresenceFrame(t *testing.T) {
	f := newLiveFixture(t, "user:alice")
	conn := f.dial(t)

	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(3*time.Second)))
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"presence"}`)))

	// A well-formed presence frame must NOT close the socket. Reading with a
	// short deadline: a timeout means the socket is healthy and simply had
	// nothing to say, which is the pass condition here.
	// Deliberately an unscaled literal, unlike the positive reads in this
	// package: a read that waits for NOTHING has to stay short. Scaling it
	// would only make the pass path slower, and wstest.Scale under a large
	// AP_TEST_TIMEOUT_SCALE would stall the suite here for no added signal.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
	_, _, err := conn.ReadMessage()
	if err != nil {
		assert.True(t, isTimeout(err), "a presence frame must be accepted, not closed on: %v", err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
