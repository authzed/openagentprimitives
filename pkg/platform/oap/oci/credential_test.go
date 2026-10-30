package oci

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func TestCredentialApplies(t *testing.T) {
	cred := &auth.Credential{Username: "ci-bot", Password: "token"}
	cases := []struct {
		name    string
		host    string
		opts    Options
		want    bool
		wantErr error
		anyErr  bool // must fail, without pinning a sentinel
	}{
		{name: "no credential: anonymous", host: "ghcr.io", opts: Options{}},
		{name: "credential without CredentialHost: error", host: "ghcr.io", opts: Options{Credential: cred}, anyErr: true},
		{name: "ref host matches CredentialHost: credential offered", host: "ghcr.io", opts: Options{Credential: cred, CredentialHost: "ghcr.io"}, want: true},
		{name: "host match is case-insensitive: credential offered", host: "GHCR.io", opts: Options{Credential: cred, CredentialHost: "ghcr.io"}, want: true},
		{name: "ref names another host: anonymous", host: "evil.example", opts: Options{Credential: cred, CredentialHost: "ghcr.io"}},
		{name: "same host, different port: anonymous", host: "ghcr.io:8443", opts: Options{Credential: cred, CredentialHost: "ghcr.io"}},
		{name: "plain HTTP to 127.0.0.1: credential offered", host: "127.0.0.1:5000", opts: Options{Credential: cred, CredentialHost: "127.0.0.1:5000", PlainHTTP: true}, want: true},
		{name: "plain HTTP to localhost: credential offered", host: "localhost:5000", opts: Options{Credential: cred, CredentialHost: "localhost:5000", PlainHTTP: true}, want: true},
		{name: "plain HTTP to [::1]: credential offered", host: "[::1]:5000", opts: Options{Credential: cred, CredentialHost: "[::1]:5000", PlainHTTP: true}, want: true},
		{name: "plain HTTP to [::1] with no port: credential offered", host: "[::1]", opts: Options{Credential: cred, CredentialHost: "[::1]", PlainHTTP: true}, want: true},
		{name: "plain HTTP to bracketed ULA with no port: ErrCredentialOverPlainHTTP", host: "[fd00::1]", opts: Options{Credential: cred, CredentialHost: "[fd00::1]", PlainHTTP: true}, wantErr: ErrCredentialOverPlainHTTP},
		{name: "plain HTTP to private LAN host: ErrCredentialOverPlainHTTP", host: "10.0.0.5:5000", opts: Options{Credential: cred, CredentialHost: "10.0.0.5:5000", PlainHTTP: true}, wantErr: ErrCredentialOverPlainHTTP},
		{name: "plain HTTP to public host: ErrCredentialOverPlainHTTP", host: "ghcr.io", opts: Options{Credential: cred, CredentialHost: "ghcr.io", PlainHTTP: true}, wantErr: ErrCredentialOverPlainHTTP},
		{name: "plain HTTP to a host the credential isn't for: anonymous, no error", host: "10.0.0.5:5000", opts: Options{Credential: cred, CredentialHost: "ghcr.io", PlainHTTP: true}},
		{name: "plain HTTP to LAN host with AllowPlainHTTPCredential: credential offered", host: "10.0.0.5:5000", opts: Options{Credential: cred, CredentialHost: "10.0.0.5:5000", PlainHTTP: true, AllowPlainHTTPCredential: true}, want: true},
		{name: "plain HTTP to compose service name with AllowPlainHTTPCredential: credential offered", host: "registry:5000", opts: Options{Credential: cred, CredentialHost: "registry:5000", PlainHTTP: true, AllowPlainHTTPCredential: true}, want: true},
		{name: "RequireCredentialHost, matching host: credential offered", host: "ghcr.io", opts: Options{Credential: cred, CredentialHost: "ghcr.io", RequireCredentialHost: true}, want: true},
		{name: "RequireCredentialHost, other host: ErrCredentialHostMismatch", host: "ghcr.io:443", opts: Options{Credential: cred, CredentialHost: "ghcr.io", RequireCredentialHost: true}, wantErr: ErrCredentialHostMismatch},
		{name: "RequireCredentialHost without a credential: anonymous, no error", host: "ghcr.io", opts: Options{RequireCredentialHost: true}},
		{name: "malformed CredentialHost: error", host: "ghcr.io", opts: Options{Credential: cred, CredentialHost: "ghcr.io "}, anyErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := credentialApplies(tc.host, tc.opts)
			switch {
			case tc.anyErr:
				require.Error(t, err)
			case tc.wantErr != nil:
				require.ErrorIs(t, err, tc.wantErr)
			default:
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
			if err != nil {
				assert.NotContains(t, err.Error(), "token", "error must not echo the secret")
			}
		})
	}
}

func TestValidateCredentialHost(t *testing.T) {
	cases := []struct {
		host    string
		wantErr bool
	}{
		{host: "ghcr.io"},
		{host: "GHCR.io"},
		{host: "localhost:5000"},
		{host: "registry:5000"},
		{host: "10.0.0.5:5000"},
		{host: "[::1]:5000"},
		{host: "", wantErr: true},
		{host: "ghcr.io ", wantErr: true},
		{host: " ghcr.io", wantErr: true},
		{host: "ghcr.io:", wantErr: true},
		{host: "ghcr.io.", wantErr: true},
		{host: "ghcr.io.:443", wantErr: true},
		{host: "ghcr.io:abc", wantErr: true},
		{host: "https://ghcr.io", wantErr: true},
		{host: "ghcr.io/org", wantErr: true},
		{host: "user@ghcr.io", wantErr: true},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("%q: valid", tc.host)
		if tc.wantErr {
			name = fmt.Sprintf("%q: rejected", tc.host)
		}
		t.Run(name, func(t *testing.T) {
			err := ValidateCredentialHost(tc.host)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// authRecorder is a registry stand-in that challenges every unauthenticated
// request with Basic auth and records each Authorization header it sees.
type authRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (a *authRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := r.Header.Get("Authorization")
	a.mu.Lock()
	a.seen = append(a.seen, h)
	a.mu.Unlock()
	if h == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (a *authRecorder) authorized() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, h := range a.seen {
		if h != "" {
			return true
		}
	}
	return false
}

// TestRepository_CredentialOnlyReachesCredentialHost drives a real resolve
// against two registries that both demand auth: the one named by
// CredentialHost must receive the credential, and the other, even though the
// ref names it, must never see an Authorization header.
func TestRepository_CredentialOnlyReachesCredentialHost(t *testing.T) {
	trusted, other := &authRecorder{}, &authRecorder{}
	trustedSrv := httptest.NewServer(trusted)
	t.Cleanup(trustedSrv.Close)
	otherSrv := httptest.NewServer(other)
	t.Cleanup(otherSrv.Close)

	opts := Options{
		PlainHTTP:      true,
		Credential:     &auth.Credential{Username: "ci-bot", Password: "token"},
		CredentialHost: strings.TrimPrefix(trustedSrv.URL, "http://"),
	}
	resolve := func(srvURL string) {
		t.Helper()
		ref := strings.TrimPrefix(srvURL, "http://") + "/agents/demo-agent:latest"
		repo, err := repository(ref, opts)
		require.NoError(t, err)
		_, err = repo.Resolve(context.Background(), repo.Reference.Reference)
		require.Error(t, err, "stand-in registry never serves a manifest")
	}

	resolve(trustedSrv.URL)
	resolve(otherSrv.URL)

	assert.True(t, trusted.authorized(), "CredentialHost must receive the credential after its challenge")
	assert.False(t, other.authorized(), "a ref on another host must be accessed anonymously, even when it challenges")
}
