package oauth

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// LoginOpts configures one full OAuth login: Discover → optional DCR →
// Run. Used by both `oap tools oauth` (manual user flow) and the
// gen-agent's mcp_oauth tool.
type LoginOpts struct {
	// HTTPClient is used for discovery, DCR, and token-exchange HTTP
	// calls. Defaults to an SSRF-guarded client (safehttp.Client()).
	HTTPClient *http.Client

	// ClientID, when non-empty, skips Dynamic Client Registration; the
	// caller has a pre-registered OAuth client. ClientSecret is paired
	// with it for confidential clients (leave empty for public).
	//
	// When ClientID is empty, Login attempts DCR if the server's
	// metadata advertises a registration_endpoint; otherwise it returns
	// an ErrNoDynamicRegistration error.
	ClientID     string
	ClientSecret string

	// Scope is forwarded to Run. Empty = use Metadata.ScopesSupported.
	Scope string

	// RedirectPort, when non-zero, forces the loopback callback
	// listener to that exact port (matching a pre-registered
	// redirect_uri). Default 0 = pick a free port; DCR registers that
	// exact URI so the auth-time and registered URIs byte-match.
	RedirectPort int

	// RedirectHost overrides the loopback host used in the auth-time
	// redirect_uri. Default empty = "127.0.0.1" (RFC 8252-preferred).
	// Set to "localhost" if the OAuth provider rejects the IP form
	// (common with consumer SaaS providers). Login always registers
	// BOTH loopback variants at DCR time, so this only affects which
	// URI is sent to /authorize. Also overridable via the
	// OAUTH_REDIRECT_HOST env var (this field wins if both are set).
	RedirectHost string

	// NoBrowser, Timeout, Out, OpenBrowser are forwarded verbatim to
	// Run. See Opts for semantics.
	NoBrowser   bool
	Timeout     time.Duration
	Out         io.Writer
	OpenBrowser func(string) error
}

// ErrNoDynamicRegistration is returned by Login when the server does
// not advertise a registration_endpoint and no ClientID was provided.
// Callers should surface a clear "register a client manually and pass
// it via ClientID" message.
type ErrNoDynamicRegistration struct{ ServerURL string }

func (e *ErrNoDynamicRegistration) Error() string {
	return fmt.Sprintf("oauth: server at %s does not advertise registration_endpoint and no ClientID was provided; register an OAuth client manually and pass it as ClientID", e.ServerURL)
}

// Login runs the full OAuth flow against mcpServerURL: discover the
// server's metadata, perform Dynamic Client Registration (if needed
// and supported), then run the PKCE auth flow. Returns the issued
// Token.
func Login(ctx context.Context, mcpServerURL string, opts LoginOpts) (*Token, error) {
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

	if opts.Out != nil {
		fmt.Fprintf(opts.Out, "Discovering OAuth endpoints for %s …\n", mcpServerURL)
	}
	meta, err := Discover(ctx, hc, mcpServerURL)
	if err != nil {
		return nil, fmt.Errorf("discover: %w", err)
	}
	if opts.Out != nil {
		fmt.Fprintf(opts.Out, "Found authorization endpoint: %s\n", meta.AuthorizationEndpoint)
	}

	port := opts.RedirectPort
	if port == 0 {
		port, err = PickFreePort()
		if err != nil {
			return nil, fmt.Errorf("pick free port: %w", err)
		}
	}
	// Register BOTH loopback variants so the server accepts either
	// auth-time redirect_uri. RFC 8252 prefers the IP form, but many
	// real-world OAuth providers only accept the hostname form
	// (or vice versa). Registering both costs nothing.
	ipURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	hostURI := fmt.Sprintf("http://localhost:%d/callback", port)

	authHost := opts.RedirectHost
	if authHost == "" {
		authHost = os.Getenv("OAUTH_REDIRECT_HOST")
	}
	if authHost == "" {
		authHost = "127.0.0.1"
	}
	if authHost != "127.0.0.1" && authHost != "localhost" {
		return nil, fmt.Errorf("oauth: RedirectHost must be \"127.0.0.1\" or \"localhost\" (got %q)", authHost)
	}

	clientID := opts.ClientID
	clientSecret := opts.ClientSecret
	if clientID == "" {
		if meta.RegistrationEndpoint == "" {
			return nil, &ErrNoDynamicRegistration{ServerURL: mcpServerURL}
		}
		if opts.Out != nil {
			fmt.Fprintln(opts.Out, "Registering client via Dynamic Client Registration …")
		}
		cid, sec, err := Register(ctx, hc, meta.RegistrationEndpoint, []string{ipURI, hostURI})
		if err != nil {
			return nil, fmt.Errorf("dynamic client registration: %w", err)
		}
		clientID = cid
		clientSecret = sec
		if opts.Out != nil {
			fmt.Fprintf(opts.Out, "Registered: client_id=%s (redirect_uris: %s, %s)\n", clientID, ipURI, hostURI)
		}
	}

	return Run(ctx, Opts{
		Metadata:     meta,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scope:        opts.Scope,
		RedirectPort: port,
		RedirectHost: authHost,
		NoBrowser:    opts.NoBrowser,
		Timeout:      opts.Timeout,
		Out:          opts.Out,
		OpenBrowser:  opts.OpenBrowser,
		HTTPClient:   hc,
	})
}

// PickFreePort opens a 127.0.0.1:0 listener, reads the OS-assigned
// port, and immediately closes the listener. Tiny race window between
// close-and-reuse but standard for local-OAuth-callback dances.
// Exported so the gen-agent's mcp_oauth tool can pre-compute the
// redirect URI shown in the user's confirm prompt (provider OAuth apps
// require the redirect_uri to be pre-whitelisted, so the user has to
// see it before approving the flow).
func PickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, err
	}
	return port, nil
}
