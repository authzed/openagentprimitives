package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRevokedPayloadRoundTrip(t *testing.T) {
	in := RevokedPayload{Kind: "tool-origin", Key: "mcpserver/linear", Scope: "team-a"}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	var out RevokedPayload
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in, out)
}

func TestKindRevokedIsValid(t *testing.T) {
	assert.True(t, KindRevoked.Valid(), "KindRevoked must be registered in kinds.go")
}
