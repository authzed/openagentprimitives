package channelevents_test

import (
	"encoding/json"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolProgressKind_ValidAndImplemented(t *testing.T) {
	require.True(t, channelevents.KindToolProgress.Valid(), "KindToolProgress must be Valid")
	require.True(t, channelevents.KindToolProgress.Implemented(), "KindToolProgress must be Implemented")
}

func TestToolProgressPayload_RoundTrip(t *testing.T) {
	pct := int32(47)
	eta := int32(30)
	in := channelevents.ToolProgressPayload{
		CallID: "tc-abc", Name: "git clone", BudgetSeconds: 300, ElapsedSeconds: 14,
		Percent: &pct, TailLine: "Receiving objects: 47%", EtaSeconds: &eta,
	}
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindToolProgress, in)
	require.NoError(t, err, "BuildEnvelope must accept KindToolProgress")

	var out channelevents.ToolProgressPayload
	require.NoError(t, json.Unmarshal(env.Payload, &out))
	assert.Equal(t, "git clone", out.Name)
	assert.Equal(t, 14, out.ElapsedSeconds)
	require.NotNil(t, out.Percent)
	assert.Equal(t, int32(47), *out.Percent)
}

func TestToolProgressPayload_DoneOmitsLayer2(t *testing.T) {
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindToolProgress,
		channelevents.ToolProgressPayload{CallID: "tc-abc", Done: true})
	require.NoError(t, err)
	// Done payloads should not carry percent/eta keys (omitempty).
	assert.NotContains(t, string(env.Payload), "percent")
	assert.NotContains(t, string(env.Payload), "etaSeconds")
}
