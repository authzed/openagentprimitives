package oauth

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/authzed/openagentprimitives/pkg/x/browser"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// Opts configures a complete OAuth 2.0 + PKCE flow.
type Opts struct {
	// Metadata is the discovered authorization-server metadata (required).
	Metadata *Metadata

	// ClientID is the OAuth client ID. If empty, Register is called first.
	ClientID string
	// ClientSecret is optional (public clients leave this empty).
	ClientSecret string

	// Scope is the space-separated scope string to request. If empty, we use
	// Metadata.ScopesSupported joined with spaces (if any).
	Scope string

	// RedirectPort is the port for the localhost callback server. 0 means
	// pick a free port automatically.
	RedirectPort int

	// RedirectHost is the host portion of the auth-time redirect_uri.
	// Default empty = "127.0.0.1" (RFC 8252-preferred). Pass "localhost"
	// to send the hostname form to /authorize for providers that reject
	// the IP literal. The listener always binds to 127.0.0.1.
	RedirectHost string

	// NoBrowser suppresses automatic browser opening.
	NoBrowser bool

	// Timeout for the entire flow (from browser open to token received).
	// Defaults to 5 minutes.
	Timeout time.Duration

	// Out is where status messages (URL, "waiting for callback …") are written.
	// If nil, os.Stderr is used.
	Out io.Writer

	// OpenBrowser is called to open the authorization URL in a browser.
	// Defaults to the platform-appropriate open command. Tests override this.
	OpenBrowser func(authURL string) error

	// HTTPClient is used for token exchange. Defaults to an SSRF-guarded client (safehttp.Client()).
	HTTPClient *http.Client
}

// callbackResult is delivered over the internal channel.
type callbackResult struct {
	code  string
	state string
	err   error
}

// Run executes the full authorization-code + PKCE flow:
//  1. Start localhost callback server.
//  2. Build PKCE values + authorization URL.
//  3. Open (or print) the authorization URL.
//  4. Wait for the callback (honors Timeout).
//  5. Validate state.
//  6. Exchange code for token.
//
// It does NOT perform discovery or client registration — callers do that
// first and supply Metadata + ClientID. Helpers (resolveScope,
// buildAuthURL, callbackErrorPage) live in flow_helpers.go.
func Run(ctx context.Context, opts Opts) (*Token, error) {
	hc := opts.HTTPClient
	if hc == nil {
		// The GUARDED client, not http.DefaultClient. This package follows URLs
		// discovered from a remote document, so an unguarded default is the
		// wrong safe-by-omission answer: a caller that forgets to pass one gets
		// an SSRF-capable client for exactly the traffic that needs a guard.
		// Every production caller already passes safehttp.Client(); this makes
		// the fallback agree with them.
		hc = safehttp.Client()
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	// 1. Start callback server.
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", opts.RedirectPort))
	if err != nil {
		return nil, fmt.Errorf("oauth flow: listen: %w", err)
	}
	port := listener.(*net.TCPListener).Addr().(*net.TCPAddr).Port
	redirectHost := opts.RedirectHost
	if redirectHost == "" {
		redirectHost = "127.0.0.1"
	}
	redirectURI := fmt.Sprintf("http://%s:%d/callback", redirectHost, port)

	resultCh := make(chan callbackResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errCode := q.Get("error"); errCode != "" {
			resultCh <- callbackResult{err: fmt.Errorf("authorization error %q: %s", errCode, q.Get("error_description"))}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(callbackErrorPage(errCode, q.Get("error_description"))))
			return
		}
		resultCh <- callbackResult{code: q.Get("code"), state: q.Get("state")}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(callbackSuccessPage))
	})

	srv := &http.Server{Handler: mux}
	safehttp.HardenServer(srv)
	go func() {
		_ = srv.Serve(listener)
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	// 2. PKCE.
	pkce, err := NewPKCE()
	if err != nil {
		return nil, fmt.Errorf("oauth flow: %w", err)
	}

	// 3. Build authorization URL.
	authURL := buildAuthURL(opts.Metadata.AuthorizationEndpoint, opts.ClientID, redirectURI, pkce, resolveScope(opts))

	// 4. Print status + open browser.
	fmt.Fprintf(out, "Open this URL in your browser:\n  %s\n\nWaiting for callback on %s ...\n", authURL, redirectURI)

	openBrowser := opts.OpenBrowser
	if openBrowser == nil {
		openBrowser = browser.Open
	}
	if !opts.NoBrowser {
		if err := openBrowser(authURL); err != nil {
			fmt.Fprintf(out, "warning: could not open browser: %v\n", err)
		}
	}

	// 5. Wait for callback (with timeout).
	flowCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var result callbackResult
	select {
	case result = <-resultCh:
	case <-flowCtx.Done():
		return nil, fmt.Errorf("oauth flow: timed out waiting for browser callback (%v)", timeout)
	}
	if result.err != nil {
		return nil, result.err
	}
	if result.state != pkce.State {
		return nil, fmt.Errorf("oauth flow: state mismatch (CSRF protection triggered)")
	}
	if result.code == "" {
		return nil, fmt.Errorf("oauth flow: callback missing authorization code")
	}

	// 6. Exchange code for token.
	tok, err := ExchangeCode(flowCtx, hc, opts.Metadata.TokenEndpoint, ExchangeCodeParams{
		Code:         result.code,
		RedirectURI:  redirectURI,
		ClientID:     opts.ClientID,
		ClientSecret: opts.ClientSecret,
		CodeVerifier: pkce.Verifier,
	})
	if err != nil {
		return nil, fmt.Errorf("oauth flow: %w", err)
	}
	// Annotate with the endpoint + client used so callers can persist them
	// for future refresh (RFC 6749 §6). These are not in the JSON body.
	tok.TokenEndpoint = opts.Metadata.TokenEndpoint
	tok.ClientID = opts.ClientID
	tok.ClientSecret = opts.ClientSecret
	return tok, nil
}
