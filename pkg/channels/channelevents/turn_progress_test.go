package channelevents_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestTurnProgressKind_ValidAndImplemented(t *testing.T) {
	require.True(t, channelevents.KindTurnProgress.Valid(), "KindTurnProgress must be Valid")
	require.True(t, channelevents.KindTurnProgress.Implemented(), "KindTurnProgress must be Implemented")
}

func TestTurnProgressPayload_RoundTrip(t *testing.T) {
	env, err := channelevents.BuildEnvelope("ns", "s1", channelevents.KindTurnProgress,
		channelevents.TurnProgressPayload{InputTokens: 1200, OutputTokens: 6400, ElapsedSeconds: 34, Seq: 3})
	require.NoError(t, err, "BuildEnvelope")
	require.NoError(t, env.Validate(), "envelope validates")

	var got channelevents.TurnProgressPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got), "unmarshal payload")
	assert.Equal(t, int64(1200), got.InputTokens)
	assert.Equal(t, int64(6400), got.OutputTokens)
	assert.Equal(t, 34, got.ElapsedSeconds)
	assert.Equal(t, 3, got.Seq)
}
