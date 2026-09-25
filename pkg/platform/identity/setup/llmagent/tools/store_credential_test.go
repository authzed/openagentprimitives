package tools_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/llmagent/tools"
)

func TestStoreCredentialRun(t *testing.T) {
	cases := []struct {
		name  string
		args  string
		check func(t *testing.T, got builtins.StoreValue)
	}{
		{
			name: "shape=bearer: captures Bearer token",
			args: `{"shape":"bearer","value":"ghp_abc"}`,
			check: func(t *testing.T, got builtins.StoreValue) {
				assert.Equal(t, "ghp_abc", got.Bearer, "Bearer")
			},
		},
		{
			name: "shape=oauth: captures all OAuth fields",
			args: `{"shape":"oauth","oauth":{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600,"token_endpoint":"https://auth.example/token","client_id":"cid","scope":"read"}}`,
			check: func(t *testing.T, got builtins.StoreValue) {
				require.NotNil(t, got.OAuth, "OAuth")
				assert.Equal(t, "at-1", got.OAuth.AccessToken, "AccessToken")
				assert.Equal(t, "rt-1", got.OAuth.RefreshToken, "RefreshToken")
				assert.Equal(t, 3600, got.OAuth.ExpiresIn, "ExpiresIn")
				assert.Equal(t, "https://auth.example/token", got.OAuth.TokenEndpoint, "TokenEndpoint")
				assert.Equal(t, "cid", got.OAuth.ClientID, "ClientID")
				assert.Equal(t, "read", got.OAuth.Scope, "Scope")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := builtins.StoreValue{}
			store := func(ctx context.Context, v builtins.StoreValue) error { got = v; return nil }

			out, err := tools.StoreCredentialRun(context.Background(), json.RawMessage(tc.args), store)
			require.NoError(t, err, "StoreCredentialRun")
			assert.Equal(t, "stored", out, "StoreCredentialRun output")
			tc.check(t, got)
		})
	}
}

func TestStoreShapeRequired(t *testing.T) {
	store := func(ctx context.Context, v builtins.StoreValue) error {
		t.Fatal("store should not be called when shape is missing")
		return nil
	}
	_, err := tools.StoreCredentialRun(context.Background(),
		json.RawMessage(`{"value":"x"}`), store)
	require.Error(t, err, "expected error for missing shape")
}
