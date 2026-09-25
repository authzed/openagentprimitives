package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolSessionKinds_ValidAndImplemented(t *testing.T) {
	for _, k := range []Kind{KindToolSessionDelta, KindToolSessionInput} {
		assert.Truef(t, k.Valid(), "%s should be Valid", k)
		assert.Truef(t, k.Implemented(), "%s should be Implemented", k)
	}
}

func TestBuildEnvelope_ToolSessionDelta(t *testing.T) {
	pl := ToolSessionDeltaPayload{
		ToolCallRef: "alice-3-tu1",
		Stream:      "stdout",
		Data:        []byte(`{"type":"assistant","text":"hi"}`),
	}
	env, err := BuildEnvelope("default", "alice", KindToolSessionDelta, pl)
	require.NoError(t, err)
	assert.Equal(t, KindToolSessionDelta, env.Kind)

	// Round-trip the payload.
	var got ToolSessionDeltaPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got))
	assert.Equal(t, pl.ToolCallRef, got.ToolCallRef)
	assert.Equal(t, pl.Stream, got.Stream)
	assert.Equal(t, pl.Data, got.Data)
}

func TestBuildEnvelope_ToolSessionDelta_Terminal(t *testing.T) {
	pl := ToolSessionDeltaPayload{
		ToolCallRef: "alice-3-tu1",
		Stream:      "stdout",
		Terminal:    true,
		ExitReason:  "idle",
		ExitCode:    -1,
	}
	env, err := BuildEnvelope("default", "alice", KindToolSessionDelta, pl)
	require.NoError(t, err)

	var got ToolSessionDeltaPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got))
	assert.True(t, got.Terminal)
	assert.Equal(t, "idle", got.ExitReason)
}

func TestBuildEnvelope_ToolSessionInput(t *testing.T) {
	pl := ToolSessionInputPayload{
		ToolCallRef: "alice-3-tu1",
		Requester:   ExternalIdentity{Kind: "slack", ExternalID: "U-bob"},
		Data:        []byte("fix the test please\n"),
	}
	env, err := BuildEnvelope("default", "alice", KindToolSessionInput, pl)
	require.NoError(t, err)
	assert.Equal(t, KindToolSessionInput, env.Kind)

	var got ToolSessionInputPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got))
	assert.Equal(t, pl.ToolCallRef, got.ToolCallRef)
	assert.Equal(t, pl.Requester.ExternalID, got.Requester.ExternalID)
	assert.Equal(t, pl.Data, got.Data)
}
