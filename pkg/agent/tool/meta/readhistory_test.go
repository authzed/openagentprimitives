package meta

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestReadHistoryTool_FormatsTranscriptResult(t *testing.T) {
	tl := NewReadHistory(ReadHistoryConfig{
		RequestSubject: "ap.session.default.sess-1.history.request",
		NATSRequest: func(_ context.Context, subject string, payload []byte) ([]byte, error) {
			assert.Equal(t, "ap.session.default.sess-1.history.request", subject,
				"tool must request on its own session's subject")
			var req channelevents.HistoryRequest
			require.NoError(t, json.Unmarshal(payload, &req))
			assert.Equal(t, 25, req.Limit)
			return json.Marshal(channelevents.HistoryResponse{
				Messages: []channelevents.HistoryResponseMessage{
					{AuthorDisplayName: "Alice", Text: "older one", TS: "4.0"},
					{AuthorDisplayName: "Bob", Text: "older two", TS: "4.1"},
				},
				OldestCursor: "4.0",
				HasMore:      true,
			})
		},
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"limit":25}`), &tool.SessionContext{})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content, "Alice: older one")
	assert.Contains(t, res.Content, "Bob: older two")
	assert.Contains(t, res.Content, "4.0")  // oldest_cursor surfaced
	assert.Contains(t, res.Content, "more") // has_more surfaced
	assert.False(t, res.Trusted, "read_thread_history surfaces third-party channel content and must stay "+
		"untrusted (Trusted:false) so the runner routes it through content-guard inspection")
}

func TestReadHistoryTool_ResponderErrorIsToolError(t *testing.T) {
	tl := NewReadHistory(ReadHistoryConfig{
		RequestSubject: "ap.session.default.sess-1.history.request",
		NATSRequest: func(context.Context, string, []byte) ([]byte, error) {
			return json.Marshal(channelevents.HistoryResponse{Error: "boom"})
		},
	})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{}`), &tool.SessionContext{})
	require.NoError(t, err, "a responder error is reported via Result.IsError, not a Go error")
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "boom")
}

func TestReadHistoryTool_TransportErrorIsToolError(t *testing.T) {
	tl := NewReadHistory(ReadHistoryConfig{
		RequestSubject: "ap.session.default.sess-1.history.request",
		NATSRequest: func(context.Context, string, []byte) ([]byte, error) {
			return nil, assert.AnError
		},
	})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{}`), &tool.SessionContext{})
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

func TestReadHistoryTool_Metadata(t *testing.T) {
	tl := NewReadHistory(ReadHistoryConfig{})
	assert.Equal(t, "read_thread_history", tl.Name())
	assert.NotEmpty(t, tl.Description())
	assert.NotEmpty(t, tl.InputSchema())
}
