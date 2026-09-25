package meta

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestReadChannelHistory(t *testing.T) {
	resp, _ := json.Marshal(channelevents.HistoryResponse{
		Messages:     []channelevents.HistoryResponseMessage{{AuthorDisplayName: "Alice", Text: "hi", TS: "1"}},
		OldestCursor: "CUR", HasMore: true,
	})
	tl := NewReadChannelHistory(ReadChannelHistoryConfig{
		RequestSubject:    "ap.session.default.foo.channel_history.request",
		SupportsDateRange: true,
		NATSRequest:       func(context.Context, string, []byte) ([]byte, error) { return resp, nil },
	})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"limit":10}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.False(t, res.Trusted, "channel history must stay untrusted (Trusted=false) so it is inspected")
	assert.Contains(t, res.Content, "Alice: hi")
	assert.Contains(t, res.Content, "oldest_cursor: CUR")

	assert.Contains(t, string(tl.InputSchema()), "since")

	// date range omitted when unsupported
	tl2 := NewReadChannelHistory(ReadChannelHistoryConfig{RequestSubject: "x", SupportsDateRange: false,
		NATSRequest: func(context.Context, string, []byte) ([]byte, error) { return resp, nil }})
	assert.NotContains(t, string(tl2.InputSchema()), "since")
}
