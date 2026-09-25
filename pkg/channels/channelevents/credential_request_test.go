package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredentialRequestKind_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindCredentialRequest.Valid(), "KindCredentialRequest should be Valid")
	assert.True(t, KindCredentialRequest.Implemented(), "KindCredentialRequest should be Implemented")
}

func TestCredentialRequestPayload_JSONRoundtrip(t *testing.T) {
	t.Run("all fields: roundtrips exactly", func(t *testing.T) {
		in := CredentialRequestPayload{
			SessionRef:         "default/sess-1",
			RecipientCanonical: "user:alice@example.com",
			LinkURL:            "https://ap.example.com/connect?token=abc123",
			Items: []CredentialRequestItem{
				{Credential: "github", Title: "GitHub", Description: "read and write your repos.", Why: "The agent needs to read your GitHub PRs."},
				{Credential: "linear-oauth", Title: "Linear", Why: "The agent needs to update Linear issues."},
			},
		}
		body, err := json.Marshal(in)
		require.NoError(t, err, "marshal")

		var got CredentialRequestPayload
		require.NoError(t, json.Unmarshal(body, &got), "unmarshal")
		assert.Equal(t, in, got, "payload should roundtrip exactly")
	})

	t.Run("Items: multi-element slice preserves order and length", func(t *testing.T) {
		in := CredentialRequestPayload{
			SessionRef: "default/sess-2",
			Items: []CredentialRequestItem{
				{Credential: "github", Title: "GitHub"},
				{Credential: "linear-oauth", Title: "Linear"},
				{Credential: "slack-oauth", Title: "Slack"},
			},
		}
		body, err := json.Marshal(in)
		require.NoError(t, err, "marshal")

		var got CredentialRequestPayload
		require.NoError(t, json.Unmarshal(body, &got), "unmarshal")
		require.Len(t, got.Items, 3, "Items should preserve length")
		assert.Equal(t, "GitHub", got.Items[0].Title, "Items[0].Title preserved")
		assert.Equal(t, "Linear", got.Items[1].Title, "Items[1].Title preserved")
		assert.Equal(t, "Slack", got.Items[2].Title, "Items[2].Title preserved")
	})

	t.Run("Items: nil slice roundtrips as nil", func(t *testing.T) {
		in := CredentialRequestPayload{
			SessionRef: "default/sess-3",
			Items:      nil,
		}
		body, err := json.Marshal(in)
		require.NoError(t, err, "marshal")

		var got CredentialRequestPayload
		require.NoError(t, json.Unmarshal(body, &got), "unmarshal")
		assert.Nil(t, got.Items, "nil Items should roundtrip as nil")
	})

	t.Run("LinkButtons: mixed OAuth + shared PAT button roundtrips", func(t *testing.T) {
		in := CredentialRequestPayload{
			SessionRef:         "default/sess-4",
			RecipientCanonical: "user:alice@example.com",
			LinkURL:            "https://ap.example.com/link?d=abc&sig=def",
			Items: []CredentialRequestItem{
				{Credential: "linear-oauth", Title: "Linear"},
				{Credential: "github-pat", Title: "GitHub PAT"},
				{Credential: "notion-pat", Title: "Notion PAT", Why: "needed for the work"},
			},
			LinkButtons: []CredentialRequestButton{
				{Label: "Connect Linear", URL: "https://ap.example.com/link/oauth/linear-oauth"},
				{Label: "Connect your accounts", URL: "https://ap.example.com/link?d=abc&sig=def"},
			},
		}
		body, err := json.Marshal(in)
		require.NoError(t, err, "marshal")

		var got CredentialRequestPayload
		require.NoError(t, json.Unmarshal(body, &got), "unmarshal")
		assert.Equal(t, in, got, "payload with LinkButtons should roundtrip exactly")
		require.Len(t, got.LinkButtons, 2, "two buttons preserved")
		assert.Equal(t, "Connect Linear", got.LinkButtons[0].Label)
		assert.Equal(t, "https://ap.example.com/link/oauth/linear-oauth", got.LinkButtons[0].URL)
	})

	t.Run("LinkButtons: nil omitted from JSON via omitempty", func(t *testing.T) {
		in := CredentialRequestPayload{
			SessionRef: "default/sess-5",
			Items: []CredentialRequestItem{
				{Credential: "github", Title: "GitHub"},
			},
		}
		body, err := json.Marshal(in)
		require.NoError(t, err, "marshal")
		assert.NotContains(t, string(body), "linkButtons",
			"nil LinkButtons must not appear in marshaled JSON (omitempty)")
	})
}
