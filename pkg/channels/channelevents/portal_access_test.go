package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPortalAccessKind_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindPortalAccess.Valid())
	assert.True(t, KindPortalAccess.Implemented())
}

func TestPortalAccessPayload_JSONRoundtrip(t *testing.T) {
	in := PortalAccessPayload{
		RecipientCanonical: "user:alice@example.com",
		LinkURL:            "https://identityd.example.org/my/accounts?d=abc&sig=def",
	}
	raw, err := json.Marshal(in)
	require.NoError(t, err)

	var out PortalAccessPayload
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, in, out)
}
