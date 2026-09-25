package tool

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArgParseError_ResultIsTrusted pins the fix for a reviewer-found gap:
// ArgParseError is the shared error-construction path every meta tool's
// Execute routes through on a malformed-args JSON decode failure. Its
// Content is 100% framework-generated (tool name + hint + the Go unmarshal
// error), so the returned Result must opt out of content-guard inspection
// via Trusted — the field the runner only consults for meta-tool results
// (see tool.go's Result.Trusted doc and loop.go's dispatchToolUses).
func TestArgParseError_ResultIsTrusted(t *testing.T) {
	res := ArgParseError("respond_to_user", `{"text": "the message body"}`, assertUnmarshalErr(t))
	assert.True(t, res.IsError)
	assert.True(t, res.Trusted, "ArgParseError's Content is framework-generated and must be Trusted")
	assert.Contains(t, res.Content, "respond_to_user")
}

// TestParseArgs_MalformedJSON_ResultIsTrusted exercises ParseArgs directly
// (the wrapper every meta tool's Execute calls first) with malformed JSON
// and asserts the returned Result carries Trusted:true via ArgParseError.
func TestParseArgs_MalformedJSON_ResultIsTrusted(t *testing.T) {
	var out struct {
		Text string `json:"text"`
	}
	res, ok := ParseArgs(json.RawMessage(`{not valid json`), &out, "query_memory", `{"kinds":["turn"]}`)
	require.False(t, ok, "malformed JSON must fail ParseArgs")
	assert.True(t, res.IsError)
	assert.True(t, res.Trusted, "a ParseArgs failure result must be Trusted (routes through ArgParseError)")
	assert.Contains(t, res.Content, "query_memory")
}

// assertUnmarshalErr produces a real json.Unmarshal error for
// TestArgParseError_ResultIsTrusted without duplicating the error's exact
// text expectations.
func assertUnmarshalErr(t *testing.T) error {
	t.Helper()
	err := json.Unmarshal([]byte(`{not valid json`), &struct{}{})
	require.Error(t, err)
	return err
}
