package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
)

const StoreCredentialName = "store_credential"
const StoreCredentialSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "value":     { "type": "string", "description": "the bearer token, kubeconfig YAML, or oauth access token" },
    "shape":     { "type": "string", "enum": ["bearer", "oauth", "kubeconfig"] },
    "oauth": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "access_token":   { "type": "string" },
        "refresh_token":  { "type": "string" },
        "expires_in":     { "type": "integer" },
        "token_endpoint": { "type": "string" },
        "client_id":      { "type": "string" },
        "scope":          { "type": "string" }
      }
    }
  },
  "required": ["shape"]
}`

// StoreCredentialRun translates the tool args to a builtins.StoreValue and calls
// store, the per-invocation callback supplied by the engine.
func StoreCredentialRun(ctx context.Context, raw json.RawMessage,
	store func(context.Context, builtins.StoreValue) error) (string, error) {

	var args struct {
		// SECRET: the single-string credential for shape=bearer or
		// shape=kubeconfig. Ignored for shape=oauth.
		Value string `json:"value"`
		// Which of the three credential shapes Value/OAuth carry; anything else
		// is refused.
		Shape string `json:"shape"`
		// The multi-key bundle for shape=oauth; nil for the other shapes.
		OAuth *struct {
			// SECRET: the bearer token itself. Required — an empty one is refused.
			AccessToken string `json:"access_token"`
			// SECRET: exchanged for a new access token later; empty means the
			// provider issued none and the credential cannot be refreshed.
			RefreshToken string `json:"refresh_token"`
			// Access-token lifetime in seconds; 0 means the provider did not say.
			ExpiresIn int `json:"expires_in"`
			// Where a refresh is redeemed; required alongside RefreshToken or the
			// credential is unrefreshable.
			TokenEndpoint string `json:"token_endpoint"`
			// OAuth client the tokens were issued to, presented on every refresh.
			ClientID string `json:"client_id"`
			// Space-separated scopes the provider actually granted.
			Scope string `json:"scope"`
		} `json:"oauth"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("store_credential: parse: %w", err)
	}
	var v builtins.StoreValue
	switch args.Shape {
	case "bearer":
		if args.Value == "" {
			return "", fmt.Errorf("store_credential: shape=bearer requires value")
		}
		v.Bearer = args.Value
	case "kubeconfig":
		if args.Value == "" {
			return "", fmt.Errorf("store_credential: shape=kubeconfig requires value")
		}
		v.KubeconfigYAML = args.Value
	case "oauth":
		if args.OAuth == nil || args.OAuth.AccessToken == "" {
			return "", fmt.Errorf("store_credential: shape=oauth requires oauth.access_token")
		}
		v.OAuth = &builtins.OAuthValue{
			AccessToken:   args.OAuth.AccessToken,
			RefreshToken:  args.OAuth.RefreshToken,
			ExpiresIn:     args.OAuth.ExpiresIn,
			TokenEndpoint: args.OAuth.TokenEndpoint,
			ClientID:      args.OAuth.ClientID,
			Scope:         args.OAuth.Scope,
		}
	default:
		return "", fmt.Errorf("store_credential: unknown shape %q", args.Shape)
	}
	if err := store(ctx, v); err != nil {
		return "", fmt.Errorf("store_credential: persist: %w", err)
	}
	return "stored", nil
}
