package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredentialLinkedKind_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindCredentialLinked.Valid(), "KindCredentialLinked should be Valid")
	assert.True(t, KindCredentialLinked.Implemented(), "KindCredentialLinked should be Implemented")
}

func TestCredentialLinkedPayload_JSONRoundtrip(t *testing.T) {
	t.Run("all fields: roundtrips exactly", func(t *testing.T) {
		in := CredentialLinkedPayload{
			RecipientCanonical: "user:YWxpY2VAZXhhbXBsZS5jb20=",
			CredentialName:     "linear-oauth",
			CredentialLabel:    "Linear OAuth",
			SessionRef:         "default/sess-1",
		}
		body, err := json.Marshal(in)
		require.NoError(t, err, "marshal")

		var got CredentialLinkedPayload
		require.NoError(t, json.Unmarshal(body, &got), "unmarshal")
		assert.Equal(t, in, got, "payload should roundtrip exactly")
	})

	t.Run("optional fields omitted when empty", func(t *testing.T) {
		in := CredentialLinkedPayload{
			RecipientCanonical: "user:YWxpY2VAZXhhbXBsZS5jb20=",
			CredentialName:     "linear-oauth",
		}
		body, err := json.Marshal(in)
		require.NoError(t, err, "marshal")

		// CredentialLabel + SessionRef use omitempty so absent fields
		// must not appear in the wire format.
		assert.NotContains(t, string(body), "credentialLabel", "absent credentialLabel must be omitted")
		assert.NotContains(t, string(body), "sessionRef", "absent sessionRef must be omitted")

		var got CredentialLinkedPayload
		require.NoError(t, json.Unmarshal(body, &got), "unmarshal")
		assert.Equal(t, in, got, "payload roundtrips")
	})
}
