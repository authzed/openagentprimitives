package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// LocalCallbackHTTPClient is the seam used for OAuth code→token exchange. The
// token_endpoint is model-supplied, so requests go through safehttp's SSRF-guarded
// dialer (bounded timeouts, private/loopback/link-local destinations refused,
// redirects re-validated per hop). Tests may override it with a plain *http.Client
// that permits loopback, to point at an httptest.Server on 127.0.0.1.
var LocalCallbackHTTPClient = safehttp.Client()

const LocalCallbackName = "local_callback"
const LocalCallbackSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "timeout_seconds": { "type": "integer", "description": "max wait, default 300" },
    "oauth_exchange": {
      "type": "object",
      "additionalProperties": false,
      "description": "OAuth authorization-code exchange config. When set and the callback delivers a code, the tool exchanges it at token_endpoint and stores the resulting token itself. The raw code is never returned either way.",
      "properties": {
        "token_endpoint": { "type": "string" },
        "client_id":      { "type": "string" },
        "client_secret":  { "type": "string", "description": "only for confidential clients" },
        "code_verifier":  { "type": "string", "description": "PKCE verifier matching the code_challenge sent on the authorize URL" }
      },
      "required": ["token_endpoint", "client_id"]
    }
  }
}`

// codeRedacted replaces any captured "code" query parameter in the returned
// params. An OAuth authorization code is a credential — it can be exchanged for
// an access token — so it must never enter LLM context.
const codeRedacted = "(redacted)"

// LocalCallbackResult is the JSON returned to the LLM.
type LocalCallbackResult struct {
	// URL is the listener address the LLM passes to open_browser as redirect_uri.
	URL string `json:"url"`
	// Params are the query parameters of the first request; "code" is always
	// replaced with "(redacted)", so the raw code never reaches the model.
	Params map[string]string `json:"params"`
	// Exchanged reports that a captured code was redeemed at the token_endpoint.
	Exchanged bool `json:"exchanged,omitempty"`
	// Stored reports the exchanged token was persisted via the engine's store
	// callback, so store_credential is NOT needed afterwards.
	Stored bool `json:"stored,omitempty"`
	// Note carries guidance when a code arrived but could not be used.
	Note string `json:"note,omitempty"`
}

// oauthExchangeArgs is the optional oauth_exchange block of the tool args.
type oauthExchangeArgs struct {
	// Where the captured authorization code is redeemed for a token.
	TokenEndpoint string `json:"token_endpoint"`
	// OAuth client the code was issued to; presented on the exchange.
	ClientID string `json:"client_id"`
	// SECRET: for confidential clients only; empty means a public (PKCE) client.
	ClientSecret string `json:"client_secret"`
	// SECRET: PKCE verifier for the code_challenge; empty when PKCE was not used.
	CodeVerifier string `json:"code_verifier"`
}

// LocalCallbackStart starts a single-shot localhost listener. Returns:
//   - url: the listener URL the caller should pass to open_browser as
//     redirect_uri (available immediately, before any request arrives).
//   - awaiter: a function that blocks until either the first HTTP request
//     lands (returning its query params) or timeout fires.
//
// The listener is closed when the awaiter returns or when ctx is cancelled.
func LocalCallbackStart(ctx context.Context, timeout time.Duration) (url string, awaiter func() (map[string]string, error), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("local_callback: listen: %w", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	listenerURL := fmt.Sprintf("http://127.0.0.1:%d/callback", addr.Port)

	captured := make(chan map[string]string, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			params := map[string]string{}
			for k, v := range r.URL.Query() {
				if len(v) > 0 {
					params[k] = v[0]
				}
			}
			fmt.Fprint(w, "callback received; you may close this window")
			select {
			case captured <- params:
			default:
			}
		}),
	}
	safehttp.HardenServer(srv)
	go func() { _ = srv.Serve(ln) }()

	await := func() (map[string]string, error) {
		// Shutdown (not Close) so the in-flight callback response is flushed
		// to the user's browser before connections drop; a short deadline still
		// guarantees the listener dies promptly even on the timeout path.
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				_ = srv.Close()
			}
		}()
		timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		select {
		case params := <-captured:
			return params, nil
		case <-timeoutCtx.Done():
			secs := int(timeout.Seconds())
			return nil, fmt.Errorf("local_callback: timed out after %ds", secs)
		}
	}
	return listenerURL, await, nil
}

// LocalCallbackRun is the LLM-tool handler. It starts the listener, writes its
// URL to stderr so the operator's log surface shows the redirect_uri before
// blocking, then waits and returns a LocalCallbackResult JSON string once a
// request arrives (or an error on timeout).
//
// Any captured "code" parameter is redacted from the result: the authorization
// code is a credential and must never enter LLM context. With oauth_exchange
// config, the tool redeems the code and persists the token via store itself.
//
// stderr may be nil; writes are skipped when it is.
func LocalCallbackRun(ctx context.Context, raw json.RawMessage, stderr io.Writer,
	store func(context.Context, builtins.StoreValue) error) (string, error) {
	var args struct {
		// How long to wait for the redirect, in seconds; 0 means the 300s default.
		TimeoutSeconds int `json:"timeout_seconds"`
		// When set, the tool redeems the captured code and stores the token itself.
		OAuthExchange *oauthExchangeArgs `json:"oauth_exchange"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	if args.TimeoutSeconds == 0 {
		args.TimeoutSeconds = 300
	}
	// Validate the exchange config before starting the listener: a bad config
	// should fail fast, not after the user completes the redirect.
	if args.OAuthExchange != nil {
		if args.OAuthExchange.TokenEndpoint == "" || args.OAuthExchange.ClientID == "" {
			return "", fmt.Errorf("local_callback: oauth_exchange requires non-empty token_endpoint and client_id")
		}
		if store == nil {
			return "", fmt.Errorf("local_callback: oauth_exchange configured but no store callback is wired (engine bug)")
		}
	}

	listenerURL, await, err := LocalCallbackStart(ctx, time.Duration(args.TimeoutSeconds)*time.Second)
	if err != nil {
		return "", err
	}
	if stderr != nil {
		fmt.Fprintf(stderr, "→ local_callback: listening on %s\n", listenerURL)
	}
	params, err := await()
	if err != nil {
		return "", err
	}

	out := LocalCallbackResult{URL: listenerURL, Params: params}
	if code := params["code"]; code != "" {
		// Redaction is scoped to "code" only: this tool models the
		// authorization-code flow, where the code is the sole credential-bearing
		// query parameter. It is not a general-purpose query-parameter redactor.
		params["code"] = codeRedacted
		if cfg := args.OAuthExchange; cfg != nil {
			tok, xerr := oauth.ExchangeCode(ctx, LocalCallbackHTTPClient, cfg.TokenEndpoint, oauth.ExchangeCodeParams{
				Code:         code,
				RedirectURI:  listenerURL,
				ClientID:     cfg.ClientID,
				ClientSecret: cfg.ClientSecret,
				CodeVerifier: cfg.CodeVerifier,
			})
			if xerr != nil {
				return "", fmt.Errorf("local_callback: code exchange failed: %w", xerr)
			}
			if serr := store(ctx, builtins.StoreValue{OAuth: &builtins.OAuthValue{
				AccessToken:   tok.AccessToken,
				RefreshToken:  tok.RefreshToken,
				ExpiresIn:     tok.ExpiresIn,
				TokenEndpoint: cfg.TokenEndpoint,
				ClientID:      cfg.ClientID,
				ClientSecret:  cfg.ClientSecret,
				Scope:         tok.Scope,
			}}); serr != nil {
				return "", fmt.Errorf("local_callback: storing exchanged token: %w", serr)
			}
			out.Exchanged = true
			out.Stored = true
		} else {
			out.Note = "an authorization code arrived but no oauth_exchange config was provided, so the code was discarded (it is never shown). Call local_callback again with oauth_exchange set (token_endpoint, client_id, optional client_secret/code_verifier) and redo the authorize redirect."
		}
	}

	j, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("local_callback: marshal result: %w", err)
	}
	return string(j), nil
}
