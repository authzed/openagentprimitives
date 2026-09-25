package slack_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

func TestBuildRestartModal_ShapeAndCallbackID(t *testing.T) {
	args := slack.RestartModalArgs{
		OriginalText:     "hello world",
		DiscardCount:     3,
		SessionNamespace: "ns",
		SessionName:      "sess",
		ChannelID:        "C1",
		ThreadTS:         "1.0",
		MessageTS:        "1.5",
	}
	view := slack.BuildRestartModal(args)
	assert.Equal(t, "modal", string(view.Type))
	assert.Equal(t, "ap_restart_modal", view.CallbackID)
	assert.NotEmpty(t, view.PrivateMetadata, "carries session + msg ref forward to view_submission")
	require.NotEmpty(t, view.Blocks.BlockSet, "modal must have at least one block")
}

func TestParseRestartModalPrivateMetadata_RoundTrip(t *testing.T) {
	args := slack.RestartModalArgs{
		SessionNamespace: "ns",
		SessionName:      "sess",
		ChannelID:        "C1",
		ThreadTS:         "1.0",
		MessageTS:        "1.5",
	}
	pm := slack.EncodeRestartPrivateMetadata(args)
	got, err := slack.DecodeRestartPrivateMetadata(pm)
	require.NoError(t, err)
	assert.Equal(t, args.SessionNamespace, got.SessionNamespace)
	assert.Equal(t, args.SessionName, got.SessionName)
	assert.Equal(t, args.ChannelID, got.ChannelID)
	assert.Equal(t, args.MessageTS, got.MessageTS)
	assert.Equal(t, args.ThreadTS, got.ThreadTS)
}

func TestBuildRestartModal_DiscardCountInHeader(t *testing.T) {
	args := slack.RestartModalArgs{
		OriginalText:     "hi",
		DiscardCount:     5,
		SessionNamespace: "ns",
		SessionName:      "s",
		ChannelID:        "C",
		MessageTS:        "1",
	}
	view := slack.BuildRestartModal(args)
	// The discard count should appear in some block's text.
	found := false
	for _, b := range view.Blocks.BlockSet {
		// Use stringification through slack-go's marshaling; quick
		// alternative is to use type-switching. Use a simple JSON
		// marshal to scan the view.
		_ = b
	}
	// Marshal the whole view to JSON and check for "5".
	raw := mustMarshal(t, view)
	assert.Contains(t, string(raw), "5", "discard count should be rendered")
	_ = found
}

func TestBuildRestartModal_SiblingFork_DifferentHeader(t *testing.T) {
	args := slack.RestartModalArgs{
		OriginalText:     "hi",
		DiscardCount:     2,
		SessionNamespace: "ns",
		SessionName:      "s",
		ChannelID:        "C",
		MessageTS:        "1",
		SiblingFork:      true,
	}
	view := slack.BuildRestartModal(args)
	raw := mustMarshal(t, view)
	assert.Contains(t, string(raw), "sibling fork", "modal header must mention sibling fork")
}

// Helper kept local to this file.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}
