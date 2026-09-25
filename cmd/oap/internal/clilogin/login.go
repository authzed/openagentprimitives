// Package clilogin owns the identity the `oap` CLI acts under: the `oap login` /
// `oap logout` commands, and EnsureIdentity — the hook every command that needs
// to name its caller goes through.
//
// Login flow:
//  1. Read the webd external-URL ConfigMap to discover the identityd base URL.
//  2. Generate a random state token; start a loopback HTTP server on :0.
//  3. Open the browser at <base>/cli/login?state=<s>&port=<p>.
//     Also print the URL so users can paste it if the browser doesn't launch.
//  4. Wait for the loopback /callback with state + one-time code (3-minute timeout).
//  5. POST <base>/cli/exchange {code, state} to exchange for a signed assertion.
//  6. Persist the assertion + claims via cliidentity.Save.
//
// The HTTP client and the callback timeout are package-level vars so tests can
// inject fakes. Browser opening is not: it goes through pkg/x/browser, which
// suppresses itself inside a test binary and offers browsertest for the tests
// that need to drive the callback. This package used to carry its own
// runtime.GOOS dispatcher — a second copy of pkg/x/browser's, which is why a
// grep for the shared helper did not find it — and an exported var a test had
// to remember to replace.
package clilogin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliidentity"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/x/browser"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// loginTimeout is the maximum time to wait for the browser callback.
// Injectable for tests.
var loginTimeout = 3 * time.Minute

// loginHTTPClient is the HTTP client used to POST /cli/exchange.
// Injectable for tests.
var loginHTTPClient = &http.Client{Timeout: 30 * time.Second}

func NewLoginCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Log in to this cluster's identity provider and cache the assertion",
		Long: `Open a browser tab to this cluster's identity provider.
After you authenticate, the signed assertion is cached in
~/.config/agentprimitives/identity.json for use by other oap commands.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			cached, err := runLoginFlow(cmd.Context(), b.Controller)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged in as %s\n", cached.Email)
			return nil
		},
	}
}

func NewLogoutCmd(_ *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the cached identity assertion",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cliidentity.Delete(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Logged out.")
			return nil
		},
	}
}

// runLoginFlow executes the full CLI login flow and saves the result.
// It is shared by NewLoginCmd and EnsureIdentity so the flow lives
// in exactly one place.
func runLoginFlow(ctx context.Context, c client.Client) (cliidentity.Cached, error) {
	baseURL, err := ResolveIdentitydBaseURL(ctx, c)
	if err != nil {
		return cliidentity.Cached{}, err
	}

	// Random 16-byte hex state.
	var rawState [16]byte
	if _, err := rand.Read(rawState[:]); err != nil {
		return cliidentity.Cached{}, fmt.Errorf("generate state: %w", err)
	}
	state := hex.EncodeToString(rawState[:])

	// Loopback listener on :0 (OS chooses port).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return cliidentity.Cached{}, fmt.Errorf("start loopback listener: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		gotState := r.URL.Query().Get("state")
		if gotState != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<html><body><p>Signed in — you can close this tab.</p></body></html>")
		// At-most-once capture: a duplicate callback (browser retry)
		// finds the buffer full and is dropped instead of blocking
		// this handler goroutine until server close.
		select {
		case codeCh <- code:
		default:
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	srv := &http.Server{Handler: mux}
	safehttp.HardenServer(srv)
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			select {
			case errCh <- serveErr:
			default:
			}
		}
	}()
	defer func() { _ = srv.Close() }()

	// Open the browser (or at least print the URL).
	loginURL := fmt.Sprintf("%s/cli/login?state=%s&port=%d", baseURL, state, port)
	fmt.Printf("Open this URL if the browser didn't launch: %s\n", loginURL)
	if berr := browser.Open(loginURL); berr != nil {
		// Non-fatal — the URL is printed above, which is what carries this
		// through SSH, CI and a headless host, and what rescues a browser that
		// opened on the wrong machine. The reason is still said out loud: a
		// user who sees no tab should not have to guess whether one was even
		// attempted, and a silent discard here is what made the equivalent
		// failure elsewhere take hours to place.
		fmt.Printf("(could not open a browser automatically: %v)\n", berr)
	}

	// Wait for the callback with a timeout.
	timeoutCtx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	var code string
	select {
	case code = <-codeCh:
	case err = <-errCh:
		return cliidentity.Cached{}, fmt.Errorf("loopback server error: %w", err)
	case <-timeoutCtx.Done():
		return cliidentity.Cached{}, fmt.Errorf("timed out waiting for browser login; please retry")
	}

	// Exchange the code for a signed assertion.
	cached, err := exchangeCode(baseURL, code, state)
	if err != nil {
		return cliidentity.Cached{}, err
	}

	if err := cliidentity.Save(cached); err != nil {
		return cliidentity.Cached{}, fmt.Errorf("save identity cache: %w", err)
	}
	return cached, nil
}

// ResolveIdentitydBaseURL reads the webd external-URL ConfigMap and returns
// the trusted-url value. Returns an actionable error when absent or empty.
func ResolveIdentitydBaseURL(ctx context.Context, c client.Client) (string, error) {
	key := types.NamespacedName{
		Namespace: externalurl.Namespace,
		Name:      spiceboxv1alpha1.WebdExternalURLConfigMap,
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("no external URL configured for this cluster; " +
				`run "oap init --local" (or configure ingress), then retry`)
		}
		return "", fmt.Errorf("get webd external-URL ConfigMap: %w", err)
	}
	url := cm.Data[spiceboxv1alpha1.WebdTrustedURLKey]
	if url == "" {
		return "", fmt.Errorf("no external URL configured for this cluster; " +
			`run "oap init --local" (or configure ingress), then retry`)
	}
	return strings.TrimRight(url, "/"), nil
}

// exchangeCode POSTs /cli/exchange to identityd and returns the Cached result.
func exchangeCode(baseURL, code, state string) (cliidentity.Cached, error) {
	body, err := json.Marshal(map[string]string{"code": code, "state": state})
	if err != nil {
		return cliidentity.Cached{}, err
	}
	resp, err := loginHTTPClient.Post(baseURL+"/cli/exchange", "application/json", bytes.NewReader(body))
	if err != nil {
		return cliidentity.Cached{}, fmt.Errorf("POST /cli/exchange: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// Surface the server's error message if present.
		var errResp struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(respBody))
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			msg = errResp.Error
		}
		return cliidentity.Cached{}, fmt.Errorf("exchange failed (HTTP %d): %s", resp.StatusCode, msg)
	}
	var exResp struct {
		Assertion   string `json:"assertion"`
		Subject     string `json:"subject"`
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
		ExpiresAt   int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal(respBody, &exResp); err != nil {
		return cliidentity.Cached{}, fmt.Errorf("decode exchange response: %w", err)
	}
	return cliidentity.Cached{
		Assertion:   exResp.Assertion,
		Subject:     exResp.Subject,
		Email:       exResp.Email,
		DisplayName: exResp.DisplayName,
		ExpiresAt:   exResp.ExpiresAt,
	}, nil
}

// EnsureIdentity returns the local user's Principal.
//
//   - Cache hit → identity.VerifiedEmail using the cached email.
//   - Miss, no ClusterIdentityProvider "default" → legacy local identity
//     (the only case the local fallback survives).
//   - Miss, CR exists → run the interactive login flow; return VerifiedEmail.
//     Login failure returns an error (no silent fallback when an IdP is configured).
func EnsureIdentity(ctx context.Context, g *apcmd.Globals) (identity.Principal, error) {
	// Fast path: a valid cached assertion answers without a cluster at all, so
	// it is checked before g.Bundle() can fail on a missing kubeconfig.
	cached, err := cliidentity.Load()
	if err != nil {
		return identity.Principal{}, fmt.Errorf("read identity cache: %w", err)
	}
	if cached != nil {
		return identity.VerifiedEmail(identity.Email(cached.Email), cached.DisplayName), nil
	}

	// Need a k8s client to check for ClusterIdentityProvider.
	b, err := g.Bundle()
	if err != nil {
		return identity.Principal{}, err
	}
	return EnsureIdentityWithClient(ctx, b.Controller)
}

// EnsureIdentityWithClient is EnsureIdentity over an already-resolved client.
// It is the whole rule; EnsureIdentity only adds the pre-cluster cache probe in
// front of it, so a caller that already holds a client — or a test that wants
// the rule without a kubeconfig — gets identical behaviour here.
func EnsureIdentityWithClient(ctx context.Context, c client.Client) (identity.Principal, error) {
	cached, err := cliidentity.Load()
	if err != nil {
		return identity.Principal{}, fmt.Errorf("read identity cache: %w", err)
	}
	if cached != nil {
		return identity.VerifiedEmail(identity.Email(cached.Email), cached.DisplayName), nil
	}

	var idp spiceboxv1alpha1.ClusterIdentityProvider
	getErr := c.Get(ctx, client.ObjectKey{Name: spiceboxv1alpha1.ClusterIdentityProviderName}, &idp)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			// No IdP configured: fall back to the legacy local identity.
			return LocalUser(), nil
		}
		return identity.Principal{}, fmt.Errorf("get ClusterIdentityProvider: %w", getErr)
	}

	// IdP is configured: require an authenticated login.
	cred, err := runLoginFlow(ctx, c)
	if err != nil {
		return identity.Principal{}, fmt.Errorf("login required: %w", err)
	}
	return identity.VerifiedEmail(identity.Email(cred.Email), cred.DisplayName), nil
}

// localOSUsername is the single source for the legacy local identity's
// username: $USER, falling back to "local-user". Consumed by LocalUser
// (Principal) and chatExternalIdentity's unverified branch (which reads
// p.ExternalID, set from this value via LocalUser()).
func localOSUsername() string {
	name := os.Getenv("USER")
	if name == "" {
		name = "local-user"
	}
	return name
}

// LocalUser returns the legacy local identity: local::<OS username>. It has no
// verified email, so it is explicitly marked synthetic-allowed — this local
// user IS the intended synthetic subject, and canonicalization must succeed.
func LocalUser() identity.Principal {
	return identity.FromExternal("local", "", identity.RawExternalID(localOSUsername()), "").AllowSynthetic()
}
