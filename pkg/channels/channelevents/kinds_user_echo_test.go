package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKindUserEcho_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindUserEcho.Valid(), "user_echo must be Valid or publishEnvelope rejects it at the publish boundary")
	assert.True(t, KindUserEcho.Implemented(), "user_echo must be Implemented or the bus client rejects it")
	assert.Equal(t, Kind("user_echo"), KindUserEcho)
}

func TestUserEchoPayloadRoundTrips(t *testing.T) {
	in := UserEchoPayload{
		Text:   "the CTA padding is wrong",
		Author: ExternalIdentity{Kind: "idp", ExternalID: "alice@example.com", Email: "alice@example.com"},
		Via:    "urn:ap:view:artifact:artifact-3f2a1b8c",
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	var out UserEchoPayload
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in, out)
}
