package nats

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A bus outage is the one failure the no-silent-errors rule cannot catch from
// above. channelsd is the sole subscriber of "ap.session.*.*.out.>"; while it
// is disconnected the server drops every outbound envelope for want of a
// subscriber, and the publishing runner's Publish still returns nil. Nothing
// upstream errors, so nothing upstream logs. The silence watchdog cannot
// back-stop it either — its stall notice rides the same bus.
//
// The connection's own lifecycle callbacks are therefore the only place the
// window can be recorded, and an operator correlating "the agent never
// replied" against logs needs the DURATION, not just a disconnect line.
func TestConnectOptionsRegistersLifecycleHandlers(t *testing.T) {
	opts, err := connectOptions(Options{URL: "nats://127.0.0.1:4222", Name: "channelsd"})
	require.NoError(t, err)
	got := applied(t, opts)

	assert.NotNil(t, got.DisconnectedErrCB,
		"a disconnect must be recorded: while it lasts, every envelope published to a subject only this connection subscribes is dropped by the server and the publisher still sees success")
	assert.NotNil(t, got.ReconnectedCB,
		"a reconnect must be recorded: it is the only line that can carry how long the gap was")
	assert.NotNil(t, got.ClosedCB,
		"a closed connection never reconnects, so every publish and subscription on it is dead — that must not be silent")
	assert.NotNil(t, got.AsyncErrorCB, "the pre-existing async error handler must survive")
}

// The three lifecycle lines, driven through the real option set. A nil *Conn is
// passed deliberately: these handlers must never dereference the connection,
// because nats.go invokes ClosedCB during teardown.
func TestLifecycleHandlersLogThroughTheOptionSet(t *testing.T) {
	opts, err := connectOptions(Options{URL: "nats://127.0.0.1:4222", Name: "channelsd"})
	require.NoError(t, err)
	got := applied(t, opts)
	read := captureSlog(t)

	require.NotNil(t, got.DisconnectedErrCB, "no disconnect handler registered")
	require.NotNil(t, got.ReconnectedCB, "no reconnect handler registered")
	require.NotNil(t, got.ClosedCB, "no closed handler registered")
	got.DisconnectedErrCB(nil, errors.New("write: broken pipe"))
	got.ReconnectedCB(nil)
	got.ClosedCB(nil)

	lines := read()
	require.Len(t, lines, 3, "each lifecycle transition must emit exactly one line")
	for _, l := range lines {
		assert.Contains(t, l, "channelsd", "every line must name the connection so an operator can grep by component")
	}
	assert.Contains(t, lines[0], "broken pipe", "the disconnect line must carry the cause")
	assert.Contains(t, lines[1], "outage", "the reconnect line must carry the outage duration")
}

// cmd/oap's connections leave Options.Name unset — including the chat TUI's
// relay, whose whole reason for indefinite reconnect is surviving a
// port-forward blip. A conn field that is empty on exactly that connection
// defeats the point of the line.
func TestUnnamedConnectionStillIdentifiesItselfInLogs(t *testing.T) {
	opts, err := connectOptions(Options{URL: "nats://127.0.0.1:4222"})
	require.NoError(t, err)
	got := applied(t, opts)
	read := captureSlog(t)

	got.DisconnectedErrCB(nil, errors.New("EOF"))
	got.ReconnectedCB(nil)

	lines := read()
	require.Len(t, lines, 2)
	for _, l := range lines {
		assert.Contains(t, l, "conn="+unnamedConn, "an unnamed connection must still say so rather than log an empty field")
	}
}

// captureSlog redirects the default slog logger — the sink the connection's
// handlers actually write to — for the duration of one test, returning a reader
// for the lines emitted so far. Mutates process-global state, so tests using it
// must not run in parallel.
func captureSlog(t *testing.T) func() []string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return func() []string {
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if l != "" {
				out = append(out, l)
			}
		}
		return out
	}
}
