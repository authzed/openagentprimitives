package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTurnActivityKind_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindTurnActivity.Valid(), "KindTurnActivity must be Valid")
	assert.True(t, KindTurnActivity.Implemented(), "KindTurnActivity must be Implemented")
	assert.Equal(t, Kind("turn_activity"), KindTurnActivity)
}

func TestTurnActivityPayload_RoundTripsThroughEnvelope(t *testing.T) {
	env, err := BuildEnvelope("default", "sess1", KindTurnActivity,
		TurnActivityPayload{Active: false, Cause: PauseCauseReply})
	require.NoError(t, err)
	require.NoError(t, env.Validate())

	var pl TurnActivityPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.False(t, pl.Active)
	assert.Equal(t, "awaiting_reply", pl.Cause)
}
