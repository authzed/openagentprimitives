// Package oauth_mcp is the builtin Go flow for the oauth-mcp provider. Drives
// OAuth 2.0 + PKCE + DCR against an MCPServer's server.url and stores
// access_token + refresh_token + expires_in + token_endpoint + client_id via the
// engine's Store callback.
//
// As a screen sequence:
//
//	authorize        register a client dynamically and authorize in the browser
//	client-id        the user's own OAuth client, when the server offers no DCR
//	client-secret    its secret, when the client is a confidential one
//	exchange         authorize again with the client the user pasted
//
// The middle two run only on servers that cannot register a client for us. That
// branch is why this is a sequence and not one screen: which questions a user is
// asked is decided by what the server said, halfway through.
package oauth_mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"
	"github.com/authzed/openagentprimitives/pkg/x/browser"
	"github.com/authzed/openagentprimitives/pkg/x/credmask"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// State keys this flow reads and writes.
//
// The first three are the flow's public vocabulary: a caller answering the
// no-DCR branch ahead of time addresses it by KeyClientID / KeyClientSecret, and
// KeyRedirectURI is what the client-credential screens announce.
//
// The rest are the minted token bundle, carried from the exchange screen to
// Result, the only place it is read. They are outputs rather than answers, so no
// screen declares them in AnswerKeys: seeding a State with an access token would
// not answer a question, it would smuggle a credential past the exchange that is
// supposed to produce one.
const (
	KeyClientID     = "client_id"
	KeyClientSecret = "client_secret"
	KeyRedirectURI  = "redirect_uri"

	keyRedirectPort = "redirect_port"
	keyRedirectHost = "redirect_host"

	KeyAccessToken = "access_token"

	keyRefreshToken        = "refresh_token"
	keyExpiresIn           = "expires_in"
	keyTokenEndpoint       = "token_endpoint"
	keyGrantedClientID     = "granted_client_id"
	keyGrantedClientSecret = "granted_client_secret"
	keyScope               = "scope"
)

// redirectHostEnv overrides the loopback host in the callback address, for
// providers that accept only the hostname form. Read here as well as by
// oauth.Login so the address announced to the user and the one sent to
// /authorize cannot disagree (see redirectHost).
const redirectHostEnv = "OAUTH_REDIRECT_HOST"

// newHTTPClient is the client factory for OAuth discovery / token exchange: the
// SSRF-guarded safehttp.Client().
var newHTTPClient = safehttp.Client

// SetHTTPClient overrides the HTTP-client factory; nil restores the SSRF-guarded
// default. For tests whose OAuth stub binds a loopback address the guarded client
// would (correctly) refuse.
func SetHTTPClient(fn func() *http.Client) {
	if fn == nil {
		newHTTPClient = safehttp.Client
		return
	}
	newHTTPClient = fn
}

// Flow is the oauth-mcp builtin.
type Flow struct{}

// New returns a new Flow.
func New() *Flow { return &Flow{} }

// Name returns the registry name for this flow.
func (Flow) Name() string { return "oauth-mcp" }

// Screens describes the flow.
//
// The server URL is resolved here rather than inside a screen so a target this
// flow cannot authorize is refused before any screen exists: the contract every
// flow answers to is that a nil error comes with at least one screen.
func (Flow) Screens(_ context.Context, req builtins.Request) ([]tui.Screen, error) {
	srv, ok := req.Target.(interface {
		MCPServer() *spiceboxv1alpha1.MCPServer
	})
	if !ok {
		return nil, fmt.Errorf("oauth-mcp: this credential is set up by authorizing an MCPServer, and %T is not one", req.Target)
	}
	cr := srv.MCPServer()
	serverURL := strings.TrimSpace(cr.Spec.Server.URL)
	if serverURL == "" {
		return nil, fmt.Errorf("oauth-mcp: MCPServer %q has an empty server.url, so there is nothing to authorize against", cr.Name)
	}

	// askedWhenNoDCR gates the two credential questions on what the authorize
	// screen discovered: a redirect URI is recorded only when the server has no
	// registration endpoint, so its absence means a client was registered
	// dynamically and the user has nothing to supply.
	askedWhenNoDCR := func(st *tui.State) bool { return !st.Has(KeyRedirectURI) }

	return []tui.Screen{
		&authorizeScreen{
			id:        "authorize",
			label:     "Authorize",
			serverURL: serverURL,
		},
		tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:       "client-id",
				Label:    "Client",
				Key:      KeyClientID,
				Title:    "OAuth client_id",
				Skip:     askedWhenNoDCR,
				Guidance: func(st *tui.State) string { return clientGuidance(st) },
				// No NoteLabel: the authorizing screen already records the client
				// that ended up holding the grant, which on this branch is this one.
			},
		}),
		tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:    "client-secret",
				Label: "Secret",
				Key:   KeyClientSecret,
				Title: "OAuth client_secret (leave empty for a public client)",
				Skip:  askedWhenNoDCR,
				// Recorded even when empty: empty is the answer that decides whether
				// renewal can authenticate — a public client refreshes on the token
				// alone, a confidential one needs this secret.
				NoteLabel: "Client secret",
				NoteValue: credmask.Mask,
			},
			Optional: true,
		}),
		&authorizeScreen{
			id:        "exchange",
			label:     "Exchange",
			serverURL: serverURL,
			manual:    true,
		},
	}, nil
}

// Result stores the bundle the exchange minted.
//
// The emptiness check is not belt-and-braces: huh's accessible renderer has no
// error channel, so a run whose input dried up arrives here as an unanswered
// State and a nil error. Storing that leaves an AgentIdentity holding an OAuth
// credential that authenticates as nobody, behind a CLI reporting success.
func (Flow) Result(ctx context.Context, req builtins.Request, st *tui.State) error {
	if st == nil {
		return errors.New("oauth-mcp: the authorization did not complete, so there is no token to store")
	}
	access := strings.TrimSpace(st.Get(KeyAccessToken))
	if access == "" {
		return errors.New("oauth-mcp: the authorization did not complete, so there is no token to store")
	}
	if req.Store == nil {
		return errors.New("oauth-mcp: nowhere to store the token")
	}

	// A malformed expires_in is dropped rather than fatal: it is a renewal
	// hint, and refusing a token that authenticates because its lifetime did
	// not parse would trade a working credential for none at all.
	expires, _ := strconv.Atoi(strings.TrimSpace(st.Get(keyExpiresIn)))

	return req.Store(ctx, builtins.StoreValue{
		OAuth: &builtins.OAuthValue{
			AccessToken:   access,
			RefreshToken:  st.Get(keyRefreshToken),
			ExpiresIn:     expires,
			TokenEndpoint: st.Get(keyTokenEndpoint),
			ClientID:      st.Get(keyGrantedClientID),
			ClientSecret:  st.Get(keyGrantedClientSecret),
			Scope:         st.Get(keyScope),
		},
	})
}

// Verify reports VerifyUnsupported: there is no live check this flow can
// perform, and saying otherwise is worse than saying nothing.
//
// A completed exchange proves the token was valid at MINT time; Verify is asked
// whether it is valid NOW. Answering "valid" from the mint-time fact would make
// credupdate.Determine refuse a genuinely dead credential as CredentialLive — on
// the default provider for every generated MCPServer.
//
// A probe is not merely missing but spec-forbidden: the access token is
// audience-bound (RFC 8707) to one MCP server, so there is no fixed endpoint,
// and a compliant resource server MUST NOT accept a token issued for another
// resource — probing the wrong one would 401 on healthy credentials and report
// them all dead. RFC 7662 introspection is optional, undiscoverable, and needs
// client authentication a probe holding only the access token does not have.
//
// Unsupported is not a warning state: consumers proceed quietly and Determine
// falls through to corroborated auth-failure observations, which is evidence we
// have rather than an assertion we never earned.
func (Flow) Verify(ctx context.Context, req builtins.VerifyRequest) (builtins.VerifyResult, error) {
	return builtins.VerifyResult{
		Status: builtins.VerifyUnsupported,
		Detail: "MCP OAuth tokens are bound to one server's audience, so there is no endpoint to check them against; stored unverified",
	}, nil
}

// authorizeScreen performs one authorization attempt: open the provider's
// consent page in the user's browser, wait for the loopback callback, and
// exchange the code it carries for a token.
//
// It asks nothing — Prepare returns no group and Apply does the work.
//
// The same type serves both attempts — the first registers a client dynamically,
// the second (reached only when the server offers no registration endpoint) uses
// the client the user pasted in between. One type rather than two is what keeps
// token-recording identical across both paths: a bundle assembled twice is a
// bundle that can be assembled two different ways.
type authorizeScreen struct {
	id, label string
	serverURL string

	// manual selects the second attempt: authorize with the client the user
	// supplied, on the port already announced to them, instead of registering one.
	manual bool

	// openedURL and openErr record what became of the attempt to open the consent
	// page, captured by the hook oauth.Login calls. Kept so a run that then goes
	// nowhere can name the browser as the reason.
	openedURL string
	openErr   error
}

func (s *authorizeScreen) ID() string { return s.id }

func (s *authorizeScreen) Label() string { return s.label }

// Prepare skips once a token is in hand and otherwise does its work in Apply.
// One rule covers both attempts: the first is skipped when the caller seeded a
// token, the second whenever the first one succeeded.
func (s *authorizeScreen) Prepare(_ context.Context, st *tui.State) (*huh.Group, error) {
	if st.Has(KeyAccessToken) {
		return nil, tui.ErrSkip
	}
	return nil, nil
}

// RequiresInteraction reports that authorizing an MCP server needs a human, and
// says why. Consulted by the CLI before a run that was told not to prompt.
//
// No flag can stand in for this step: the provider's consent page has to be
// opened in a browser and approved by the person whose account is being
// delegated, and the token that comes back is minted by that approval. Refusing
// with "supply it via flags" would send the user hunting for a flag that cannot
// exist; proceeding would open a browser tab at somebody who is not there.
func (s *authorizeScreen) RequiresInteraction(st *tui.State) string {
	if st.Has(KeyAccessToken) {
		return ""
	}
	return "authorizing an MCP server cannot be automated: the provider's consent page has to be " +
		"opened in a browser and approved by the account holder, and the token is minted by that " +
		"approval. Re-run without asking to skip prompts."
}

func (s *authorizeScreen) Apply(ctx context.Context, st *tui.State) error {
	opts := oauth.LoginOpts{
		HTTPClient: newHTTPClient(),
		// Discarded rather than shown: oauth.Login narrates progress to a stream
		// and a flow owns none — the sequencer renders groups, not a log. The one
		// line of that narration a user could act on, the consent URL when the
		// browser refuses to open, cannot be a note either: it is several hundred
		// characters of PKCE challenge and state, and a note truncates at the
		// form's column budget. Losing it whole beats showing half. See openErr.
		Out:         io.Discard,
		OpenBrowser: s.open,
	}
	if s.manual {
		// Pinned, not re-derived: the user was shown this exact redirect URI so
		// they could allow it on their OAuth app, and a provider matches it byte
		// for byte. Both halves are pinned — a fresh port would fail every run of
		// this branch, and a re-derived HOST would fail only for the users who set
		// OAUTH_REDIRECT_HOST, which is a far worse way to be wrong.
		port, err := strconv.Atoi(st.Get(keyRedirectPort))
		if err != nil {
			return fmt.Errorf("oauth-mcp: the callback address announced earlier could not be reused: %w", err)
		}
		opts.RedirectPort = port
		opts.RedirectHost = st.Get(keyRedirectHost)
		opts.ClientID = strings.TrimSpace(st.Get(KeyClientID))
		opts.ClientSecret = strings.TrimSpace(st.Get(KeyClientSecret))
		if opts.ClientID == "" {
			return errors.New("oauth-mcp: no OAuth client_id was supplied, and this server cannot register one for us")
		}
	}

	tok, err := oauth.Login(ctx, s.serverURL, opts)
	if err != nil {
		var noDCR *oauth.ErrNoDynamicRegistration
		if s.manual || !errors.As(err, &noDCR) {
			return s.describe(err)
		}
		// Pre-registered-app path. The whole redirect URI is fixed HERE, before the
		// user is asked for anything, so the address they are told to allow is the
		// one the second attempt will bind.
		port, perr := oauth.PickFreePort()
		if perr != nil {
			return fmt.Errorf("oauth-mcp: could not reserve a local callback port: %w", perr)
		}
		host, herr := redirectHost()
		if herr != nil {
			return herr
		}
		st.Set(keyRedirectPort, strconv.Itoa(port))
		st.Set(keyRedirectHost, host)
		st.Set(KeyRedirectURI, fmt.Sprintf("http://%s:%d/callback", host, port))
		return nil
	}

	s.record(st, tok)
	return nil
}

// redirectHost resolves the loopback host the callback will be reached on,
// by the same rule oauth.Login applies to an unset LoginOpts.RedirectHost.
//
// Duplicating that rule is the point. Login resolves the host at authorize time,
// well after this flow has printed an address for the user to register on their
// OAuth app; leaving it to resolve itself means announcing 127.0.0.1 and then
// sending localhost to /authorize whenever OAUTH_REDIRECT_HOST is set — a
// redirect_uri mismatch, the same failure the port pinning prevents. Resolving it
// here and pinning it back onto Login is what makes the two agree.
//
// An unusable value is refused rather than quietly replaced: Login would reject
// it a moment later anyway, and doing it here names the variable that set it,
// before a browser opens.
func redirectHost() (string, error) {
	host := os.Getenv(redirectHostEnv)
	switch host {
	case "":
		return "127.0.0.1", nil
	case "127.0.0.1", "localhost":
		return host, nil
	default:
		return "", fmt.Errorf("oauth-mcp: %s must be \"127.0.0.1\" or \"localhost\" (got %q)", redirectHostEnv, host)
	}
}

// open is the hook oauth.Login calls to show the consent page. It records what
// happened and passes the error back unchanged, so Login keeps waiting on the
// callback: an opener that reports failure has not necessarily failed to open a
// browser, and cutting the wait short would abandon runs about to succeed.
func (s *authorizeScreen) open(u string) error {
	s.openedURL = u
	s.openErr = browser.Open(u)
	return s.openErr
}

// describe turns a failed attempt into something the user can act on. A browser
// that never opened is reported as the cause rather than left for the user to
// infer from a timeout: the consent page is the only way this flow can complete,
// so a failure to show it explains every symptom that follows.
func (s *authorizeScreen) describe(err error) error {
	if s.openErr != nil {
		return fmt.Errorf("oauth-mcp: the consent page could not be opened in a browser (%v), so the authorization was never approved: %w", s.openErr, err)
	}
	if s.manual {
		return fmt.Errorf("oauth-mcp: authorizing with the client_id you supplied failed: %w", err)
	}
	return fmt.Errorf("oauth-mcp: authorization failed: %w", err)
}

// record writes the minted bundle into State for Result to read, and the
// summary lines for the user to read.
//
// The access token is masked and the client secret omitted outright: the summary
// goes to scrollback, outliving the terminal the credential was minted into, and
// it is a plain io.Writer, so a `> setup.log` redirect captures it too. The
// client_id is shown in full — it names which OAuth client holds the grant, and
// it is not a secret.
func (s *authorizeScreen) record(st *tui.State, tok *oauth.Token) {
	st.Set(KeyAccessToken, tok.AccessToken)
	st.Set(keyRefreshToken, tok.RefreshToken)
	st.Set(keyExpiresIn, strconv.Itoa(tok.ExpiresIn))
	st.Set(keyTokenEndpoint, tok.TokenEndpoint)
	st.Set(keyGrantedClientID, tok.ClientID)
	st.Set(keyGrantedClientSecret, tok.ClientSecret)
	st.Set(keyScope, tok.Scope)

	st.Note("MCP server", s.serverURL)
	if tok.ClientID != "" {
		st.Note("Authorized client", tok.ClientID)
	}
	st.Note("Access token", credmask.Mask(tok.AccessToken))
}

// clientGuidance is what the user reads above the client_id field: why they are
// being asked, and the redirect URI their OAuth app has to allow.
//
// The redirect URI is the load-bearing line. Provider OAuth apps refuse any
// redirect_uri not registered on the app beforehand, so a user who pastes
// credentials without whitelisting it first gets an opaque provider failure on
// the next screen. It is short enough to survive a note whole.
func clientGuidance(st *tui.State) string {
	var b strings.Builder
	b.WriteString("This server can't register an OAuth client for us,\n")
	b.WriteString("so it needs one of your own.\n\n")
	b.WriteString("Register an app with the provider, allow this exact\nredirect URI on it, then paste its credentials:\n")
	b.WriteString("  " + st.Get(KeyRedirectURI) + "\n")
	return strings.TrimRight(b.String(), "\n")
}
