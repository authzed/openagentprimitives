package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutboundUserMessagePayloadComponents(t *testing.T) {
	t.Run("present when set: round-trips", func(t *testing.T) {
		p := OutboundUserMessagePayload{Text: "hi", Components: json.RawMessage(`[{"type":"divider"}]`)}
		b, err := json.Marshal(p)
		require.NoError(t, err)
		assert.Contains(t, string(b), `"components"`)

		var back OutboundUserMessagePayload
		require.NoError(t, json.Unmarshal(b, &back))
		assert.Equal(t, "hi", back.Text)
		assert.JSONEq(t, `[{"type":"divider"}]`, string(back.Components))
	})

	t.Run("omitted when empty", func(t *testing.T) {
		b, err := json.Marshal(OutboundUserMessagePayload{Text: "hi"})
		require.NoError(t, err)
		assert.NotContains(t, string(b), "components")
	})
}
