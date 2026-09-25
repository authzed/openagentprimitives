package interact

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// sentAppToolCall decodes the AppToolCallRequest the fake runner transport
// received, so a test can assert on what the SERVER sent rather than on what
// the browser asked for.
func sentAppToolCall(t *testing.T, d *appToolCallFakeDeps) channelevents.AppToolCallRequest {
	t.Helper()
	require.NotNil(t, d.gotRequestBody, "runner responder must have been invoked")
	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(d.gotRequestBody, &env))
	var sent channelevents.AppToolCallRequest
	require.NoError(t, json.Unmarshal(env.Payload, &sent))
	return sent
}

// A widget's HTML is authored by an MCP server and runs its own script, and the
// runner's app-tool registry is one flat map across every origin the session
// has. So the tool NAME alone decided which server a widget reached.
//
// The browser is allowed to say which widget is calling — it has the artifact
// id already — but not what that widget is. The origin comes off the session's
// own status.activeWidgets, server-side, and rides the envelope as
// WidgetOrigin.
func TestAppToolCallStampsTheWidgetOriginFromSessionStatus(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true, widgetOrigins: map[string]string{
		"art-1": "mcpserver/billing",
	}}

	rec := postAppToolCall(t, d, "user:viewer",
		`{"toolName":"billing_read","args":{},"artifactId":"art-1","widgetOrigin":"mcpserver/evil"}`)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	sent := sentAppToolCall(t, d)
	assert.Equal(t, "mcpserver/billing", sent.WidgetOrigin,
		"the origin is resolved from the session's own widget list, never taken from the body")
}

// An artifact id that resolves to no widget on THIS session is either stale or
// forged. Falling back to an unpinned call would make "send a bogus artifact
// id" the way around the pin, so it is refused before any runner round-trip.
func TestAppToolCallUnknownWidgetArtifactIsRefusedAndNeverReachesTheRunner(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true, widgetOrigins: map[string]string{}}
	d.nats = func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		t.Fatal("must not reach the runner for a widget this session does not have")
		return nil, nil
	}

	rec := postAppToolCall(t, d, "user:viewer",
		`{"toolName":"billing_read","args":{},"artifactId":"art-nope"}`)

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

// A status read that fails leaves the widget's identity UNKNOWN, which is not
// the same as "no widget" — proceeding unpinned would turn an outage into a
// bypass.
func TestAppToolCallWidgetOriginLookupErrorFailsClosed(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true, widgetOriginErr: true}
	d.nats = func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		t.Fatal("must not reach the runner when the widget's identity is unknown")
		return nil, nil
	}

	rec := postAppToolCall(t, d, "user:viewer",
		`{"toolName":"billing_read","args":{},"artifactId":"art-1"}`)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
}

// The counterweight: an agent-declared UI binding is not a widget. It sends no
// artifact id, and the runner leaves such a call unpinned — so the lookup must
// not run at all, and WidgetOrigin must stay empty rather than becoming some
// arbitrary widget's.
func TestAppToolCallWithoutAnArtifactIsUnpinned(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true, widgetOrigins: map[string]string{"art-1": "mcpserver/billing"}}

	rec := postAppToolCall(t, d, "user:viewer", `{"toolName":"billing_read","args":{}}`)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Empty(t, sentAppToolCall(t, d).WidgetOrigin,
		"a call with no widget behind it carries no origin")
	assert.Equal(t, 0, d.widgetOriginCalls, "no artifact id means nothing to resolve")
}
