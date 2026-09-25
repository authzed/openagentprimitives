package channelevents_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestThreadTitlePayload_RoundTrip(t *testing.T) {
	in := channelevents.ThreadTitlePayload{Title: "Refactoring the auth layer", Emoji: "🔧"}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"title":"Refactoring the auth layer"`)
	assert.Contains(t, string(b), `"emoji":"🔧"`)

	var out channelevents.ThreadTitlePayload
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in, out)
}

func TestThreadTitlePayload_EmojiOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(channelevents.ThreadTitlePayload{Title: "x"})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "emoji")
}
