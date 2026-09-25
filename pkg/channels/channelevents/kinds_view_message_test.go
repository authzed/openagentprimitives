package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKindViewMessage_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindViewMessage.Valid(), "view_message must be Valid or publishEnvelope rejects it at the publish boundary (envelope.go:86)")
	assert.True(t, KindViewMessage.Implemented(), "view_message must be Implemented or the bus client rejects it at publish time")
	assert.Equal(t, Kind("view_message"), KindViewMessage)
}

func TestViewMessagePayloadRoundTrips(t *testing.T) {
	in := ViewMessagePayload{
		Text: "the CTA padding is wrong",
		Author: ExternalIdentity{
			Kind: "idp", ExternalID: "alice@example.com", Email: "alice@example.com",
		},
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)

	var out ViewMessagePayload
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in, out)
}

func TestViewMessageResultPayloadRoundTrips(t *testing.T) {
	in := ViewMessageResultPayload{
		Outcome:              "routed",
		RequesterCanonicalID: "YWxpY2VAZXhhbXBsZS5jb20",
		NewSession:           true,
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)

	var out ViewMessageResultPayload
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in, out)
}

// The Error field is how a handler-side failure reaches the requester instead
// of the requester waiting out its timeout. See AGENTS.md, no-silent-errors.
func TestViewMessageResultCarriesHandlerError(t *testing.T) {
	b, err := json.Marshal(ViewMessageResultPayload{Error: "get session: not found"})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"error":"get session: not found"`)
}
