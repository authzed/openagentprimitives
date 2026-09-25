package bronzethread_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// unorderedResult is the shape a real MCP server returns and a Go map cannot
// carry: the keys are in the server's own order, which is not alphabetical, and
// the id is larger than a float64 can hold exactly.
const unorderedResult = `{"resource":"permissionsystems","namespaced":true,"session_id":"AFCE52","count":9007199254740993}`

// TestCannedHandler_ServesTheRecordedBytes is the defect this exists to close.
//
// The driver used to decode a recorded result into map[string]any and let the
// stub re-encode it, which SORTS the keys and rounds a large integer through
// float64. The Expect the capture derived from the recorded text could then
// never match the text the replay served, on any bundle whose tool answered
// with more than one key — a divergence report on a replay that took the
// identical path.
func TestCannedHandler_ServesTheRecordedBytes(t *testing.T) {
	h := bt.CannedHandler(json.RawMessage(unorderedResult))
	require.NotNil(t, h)

	got, err := json.Marshal(h(map[string]any{}))
	require.NoError(t, err)
	assert.Equal(t, unorderedResult, string(got),
		"the replay must hand the model the bytes the run recorded, key order and integer precision included")
}

// TestServedResult_IsWhatTheHandlerActuallyProduces is the pairing that keeps a
// derived Expect true: the capture predicts the served text with ServedResult,
// the replay produces it through CannedHandler, and the two must not drift.
//
// HTML escaping is the case that motivates having a predictor at all. The stub
// marshals with the standard encoder, which rewrites <, > and & inside strings
// even when it is only passing a RawMessage through — so the served text is NOT
// the recorded text, and an Expect copied from the transcript would never match
// for any tool whose output mentions a tag or an ampersand.
func TestServedResult_IsWhatTheHandlerActuallyProduces(t *testing.T) {
	cases := map[string]string{
		"keys in the server's order, and an id past 2^53": unorderedResult,
		"HTML characters inside a string":                 `{"s":"a<b&c>d"}`,
		"already-canonical bytes are unchanged":           `{"a":1,"b":2}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			predicted, ok := bt.ServedResult(json.RawMessage(in))
			require.True(t, ok, "valid JSON must be predictable")

			actual, err := json.Marshal(bt.CannedHandler(json.RawMessage(in))(map[string]any{}))
			require.NoError(t, err)
			assert.Equal(t, string(actual), string(predicted),
				"the capture's prediction and the replay's output are one contract")
		})
	}
}

// Non-JSON is left exactly as it is, and says so. A sandbox tool's recovered
// stdout is plain text: predicting a marshalled form for it would put quotes
// around an assertion the exec binder never wraps.
func TestServedResult_NonJSONIsUnchanged(t *testing.T) {
	got, ok := bt.ServedResult(json.RawMessage("no open pull requests\n"))
	assert.False(t, ok)
	assert.Equal(t, "no open pull requests\n", string(got))
}

func TestSequencedHandler_ServesTheRecordedBytes(t *testing.T) {
	h := bt.SequencedHandler([]json.RawMessage{json.RawMessage(unorderedResult)})
	require.NotNil(t, h)

	got, err := json.Marshal(h(map[string]any{}))
	require.NoError(t, err)
	assert.Equal(t, unorderedResult, string(got),
		"a sequenced result is served as recorded for the same reason a constant one is")
}

// A value that will not decode is a malformed bundle. The handler answers with
// an error the model can report rather than a silent null that reads as an
// empty result — and it must NOT pass the undecodable bytes through, which
// would corrupt the whole JSON-RPC response instead of one tool's result.
func TestCannedHandler_UndecodableValueBecomesAnErrorResult(t *testing.T) {
	cases := map[string]func(json.RawMessage) func(map[string]any) any{
		"constant":  bt.CannedHandler,
		"sequenced": func(v json.RawMessage) func(map[string]any) any { return bt.SequencedHandler([]json.RawMessage{v}) },
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			h := build(json.RawMessage(`{"broken":`))
			require.NotNil(t, h)
			got, err := json.Marshal(h(map[string]any{}))
			require.NoError(t, err, "the handler's own result must always marshal")
			assert.Contains(t, string(got), "undecodable recorded result")
		})
	}
}

// TestCannedHandler_EmptyIsNil mirrors SequencedHandler: nothing recorded means
// nothing registered, so the stub answers "tool not registered" and names it.
func TestCannedHandler_EmptyIsNil(t *testing.T) {
	assert.Nil(t, bt.CannedHandler(nil))
}
