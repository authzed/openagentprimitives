package agentui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// goldenActionEndpointPath is the ONE artifact both halves of the actions
// ENDPOINT seam (J4 — this task's own join) read. It is a SEPARATE file from
// every other golden in this package, deliberately:
//
//   - ui/testdata/props.golden.json pins the GET bootstrap seam (Plan 3).
//   - ui/testdata/bindings.golden.json pins the POST .../bindings seam
//     (Plan 4).
//   - ui/testdata/props.actions.golden.json pins the DECLARATION's action
//     TABLE shape — what ViewFor sends the browser for
//     AgentUISpec.Actions (Plan 5 Task 2's own join, J3).
//   - This file pins the actionRequestBody/actionResponseBody WIRE SHAPE
//     of POST .../actions — a request the browser sends and the response
//     it parses. None of the four fixtures, byte layouts, or existing
//     assertions above should ever need to move to accommodate another;
//     merging any two would force an edit to one contract to churn the
//     other's fixture and tests too (see the sibling goldenActionsPath's
//     own doc comment in props_golden_internal_test.go for the same
//     argument made about J3 vs the bootstrap seam).
//
// Two fields, both load-bearing on both sides:
//
//	request  — the POST body the browser sends. Decoded here through
//	           actionRequestBody (so a json-tag rename yields an empty
//	           action/params/inputs and fails), asserted byte-for-byte as
//	           the body @ap/agentui's ActionRequestBody type names.
//	response — the REAL actionsHandler's response body for `request`,
//	           MINUS its per-request-random requestId (see
//	           TestActionEndpointMatchesTheGoldenTheBrowserParses for why
//	           that one field is compared separately rather than pinned).
//	           Read through @ap/agentui's ActionResponseBody type on the TS
//	           side.
//
// # Why this exists
//
// This branch has shipped the exact defect shape three times already (see
// the standing agent-ui briefing's §3): a contract split across a Go task
// and a TS task, each reviewed alone, drifts invisibly because a literal
// spelled out independently in each language's own test is NOT a pin — a
// COORDINATED rename (change the Go json tag, update the Go test's own
// literal to match, regenerate this file to match) leaves every existing
// suite green while the browser silently stops being able to read the
// field. With this file in place there is no edit to one side alone that
// keeps both suites green: change actionResponseBody's tags and this test
// fails; regenerate the golden to match and the TS suite
// (ui/actionsGolden.test.tsx) fails instead, because it asserts the parsed
// field BY NAME through the TS type rather than echoing the file's own
// value back at itself.
//
// To change either contract deliberately: update actions.go, this file's
// expectations, the golden, AND @ap/agentui's ActionRequestBody/
// ActionResponseBody (web/packages/agentui/src/types.ts) together.
const goldenActionEndpointPath = "ui/testdata/actions.golden.json"

// goldenActionEndpoint is the golden's own shape. Both fields stay
// json.RawMessage: this file must compare the handler's bytes against the
// file's bytes, and decoding either through a Go struct on the way would
// launder exactly the tag renames the comparison exists to catch.
type goldenActionEndpoint struct {
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response"`
}

func readActionEndpointGolden(t *testing.T) goldenActionEndpoint {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(goldenActionEndpointPath))
	require.NoError(t, err, "the golden the frontend suite reads must exist")
	var g goldenActionEndpoint
	require.NoError(t, json.Unmarshal(raw, &g), "the golden must be well-formed JSON")
	return g
}

// TestActionEndpointMatchesTheGoldenTheBrowserParses pins actionRequestBody
// (the request half) and actionResponseBody (the response half) — the json
// tags AND the state/message literals — by asserting the REAL
// actionsHandler's bytes against the file the TS suite reads. It also pins
// the request in the one direction a Go test can: decoding the golden's
// request body through actionRequestBody must still yield the values the
// fixture declaration's own action needs.
//
// requestId is compared separately (non-empty, then stripped) rather than
// pinned byte-for-byte: unlike every other field here, it is a fresh
// crypto/rand value on every real invocation (newActionRequestID) — pinning
// it would either make this test flaky or force it to fake random-number
// generation, neither of which is what this join is protecting.
func TestActionEndpointMatchesTheGoldenTheBrowserParses(t *testing.T) {
	g := readActionEndpointGolden(t)

	// The request half. Decoding the golden's bytes (not re-marshalling the
	// struct) is what makes a field-tag rename visible: the field would
	// simply not be populated.
	var body actionRequestBody
	require.NoError(t, json.Unmarshal(g.Request, &body), "the golden's request must decode")
	assert.Equal(t, "advance", body.Action,
		"actionRequestBody's shape changed: update actions.go, %s, and ActionRequestBody together", goldenActionEndpointPath)
	assert.Equal(t, map[string]string{"lead": "l-1"}, body.Params)
	assert.Equal(t, map[string]string{"why": "ready"}, body.Inputs)

	d := &actionsFakeDeps{interactOK: true, k8s: newActionsFakeK8s(t, actionsBaseObjs()...), origin: actionsOrigin,
		nats: func(_ string, data []byte, _ time.Duration) ([]byte, error) {
			var env channelevents.Envelope
			if err := json.Unmarshal(data, &env); err != nil {
				return nil, err
			}
			var req channelevents.UIActionRequest
			if err := json.Unmarshal(env.Payload, &req); err != nil {
				return nil, err
			}
			return json.Marshal(channelevents.UIActionResponse{
				RequestID: req.RequestID, State: actionsDefaultState, Message: actionsDefaultMessage,
			})
		}}
	rec := doPostAction(t, d, actionsSubject, actionsOrigin, string(g.Request))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var gotFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &gotFields))
	idRaw, hasID := gotFields["requestId"]
	require.True(t, hasID, "actionResponseBody must always carry a requestId field")
	var id string
	require.NoError(t, json.Unmarshal(idRaw, &id))
	assert.NotEmpty(t, id, "the per-request correlation id must never be empty")
	delete(gotFields, "requestId")
	gotRest, err := json.Marshal(gotFields)
	require.NoError(t, err)

	assert.JSONEq(t, string(g.Response), string(gotRest),
		"the actions response envelope changed: update actions.go, %s, and ActionResponseBody together", goldenActionEndpointPath)
}
