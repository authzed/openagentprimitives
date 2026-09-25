package channelevents

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUIViewUpdateKind_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindUIViewUpdate.Valid(), "KindUIViewUpdate must be Valid")
	assert.True(t, KindUIViewUpdate.Implemented(), "KindUIViewUpdate must be Implemented")
	assert.Equal(t, Kind("ui_view_update"), KindUIViewUpdate)
}

func TestUIViewUpdatePayload_RoundTripsThroughEnvelope(t *testing.T) {
	at := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	env, err := BuildEnvelope("demo-ns", "demo-session", KindUIViewUpdate,
		UIViewUpdatePayload{Hook: "panel", UpdatedAt: at})
	require.NoError(t, err)
	require.NoError(t, env.Validate())

	var pl UIViewUpdatePayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "panel", pl.Hook)
	assert.True(t, at.Equal(pl.UpdatedAt))
}
