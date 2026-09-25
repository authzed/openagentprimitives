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

// AppToolCallResponse.Message is the OPERATOR channel, and its own doc comment
// says so: "it is NOT browser-safe... they name SpiceDB permissions, resource
// types and IDs, CRD field names, and the raw subject." ViewerMessage is the
// half a replying site authored for a human to read, and a site that has none
// leaves it empty so the renderer substitutes its own fixed copy.
//
// The handler relayed the runner's reply verbatim, so Message reached the
// widget — and a widget is HTML an MCP server wrote. A denial's message
// therefore handed a hostile server the permission that was checked, the
// resource id it was checked against, and the acting subject: a map of the
// authorization model, delivered by the very control that refused it.
//
// The response is now filtered at the boundary rather than at each replying
// site, so a status added later cannot forget.

func TestAppToolCall_OperatorMessageIsNotRelayedToTheWidget(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true}
	d.nats = func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		return json.Marshal(channelevents.AppToolCallResponse{
			Status:        channelevents.AppToolCallStatusDenied,
			Message:       `denied: user:YWxpY2VAY29ycC5leGFtcGxl lacks "view" on issue:ENG-1 (spec.authz.toolCalls)`,
			ViewerMessage: "You do not have access to that item.",
		})
	}

	rec := postAppToolCall(t, d, "user:viewer", `{"toolName":"gh_read","args":{}}`)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "YWxpY2VAY29ycC5leGFtcGxl", "the acting subject must not reach the widget")
	assert.NotContains(t, body, "issue:ENG-1", "nor the resource that was checked")
	assert.NotContains(t, body, "spec.authz.toolCalls", "nor the CRD field that governs it")
	assert.Contains(t, body, "You do not have access to that item.",
		"the copy the replying site authored FOR a human still reaches them")
}

// A reply with no viewer copy must not fall back to the operator message —
// that is exactly the leak, and the renderer already substitutes its own fixed
// text for an empty ViewerMessage.
func TestAppToolCall_NoViewerMessageMeansNoMessageAtAll(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true}
	d.nats = func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		return json.Marshal(channelevents.AppToolCallResponse{
			Status:  channelevents.AppToolCallStatusError,
			Message: "dial tcp 10.4.2.9:8080: connect: connection refused",
		})
	}

	rec := postAppToolCall(t, d, "user:viewer", `{"toolName":"gh_read","args":{}}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "10.4.2.9", "an internal address is not viewer copy")

	var got channelevents.AppToolCallResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got.Message, "the operator channel is dropped, not downgraded into the viewer one")
	assert.Equal(t, channelevents.AppToolCallStatusError, got.Status,
		"the STATUS still travels — the widget must be able to act on the outcome")
}

// A successful call's Result is the whole point of the route and must pass
// through untouched.
func TestAppToolCall_SuccessfulResultStillReachesTheWidget(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true}
	d.nats = func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		return json.Marshal(channelevents.AppToolCallResponse{
			Status: channelevents.AppToolCallStatusOK,
			Result: json.RawMessage(`{"rows":3}`),
		})
	}

	rec := postAppToolCall(t, d, "user:viewer", `{"toolName":"gh_read","args":{}}`)

	require.Equal(t, http.StatusOK, rec.Code)
	var got channelevents.AppToolCallResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.JSONEq(t, `{"rows":3}`, string(got.Result))
}
