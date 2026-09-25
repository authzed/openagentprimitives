package oidckind_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/oidckind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/oidckind/oidctest"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
)

const (
	testClientID     = "test-client-id"
	testClientSecret = "test-secret"
	testState        = "rand-state-abc123"
	testRedirectURL  = "https://example.com/oidc/callback/idp"
)

// newProvider creates an oidc provider backed by the fake issuer.
func newProvider(t *testing.T, fi *oidctest.FakeIssuer, opts oidckind.Options) idp.Provider {
	t.Helper()
	ctx := gooidc.ClientContext(context.Background(), fi.Client())
	p, err := oidckind.NewWithOptions(ctx, idp.Config{
		Issuer:       fi.URL(),
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		RedirectURL:  testRedirectURL,
	}, opts)
	require.NoError(t, err, "NewWithOptions must succeed")
	return p
}

// completeWith calls Complete using the fake issuer's test server client.
func completeWith(t *testing.T, fi *oidctest.FakeIssuer, p idp.Provider, cb idp.CallbackParams) (identity.Principal, *idp.TokenSet, error) {
	t.Helper()
	ctx := gooidc.ClientContext(context.Background(), fi.Client())
	return p.Complete(ctx, cb)
}

// ---- Begin tests ----------------------------------------------------------------

func TestBegin_AuthorizeURLContainsRequiredParams(t *testing.T) {
	fi := oidctest.New()
	t.Cleanup(fi.Server.Close)
	p := newProvider(t, fi, oidckind.Options{})

	authURL, err := p.Begin(context.Background(), testState)
	require.NoError(t, err)

	u, err := url.Parse(authURL)
	require.NoError(t, err, "authorize URL must be valid")

	q := u.Query()
	assert.Equal(t, testClientID, q.Get("client_id"), "client_id")
	assert.Equal(t, testRedirectURL, q.Get("redirect_uri"), "redirect_uri")
	assert.Equal(t, testState, q.Get("state"), "state")
	assert.Equal(t, testState, q.Get("nonce"), "nonce must equal state")

	scopes := strings.Split(q.Get("scope"), " ")
	assert.Contains(t, scopes, "openid", "scope must contain openid")
	assert.Contains(t, scopes, "email", "scope must contain email")
	assert.Contains(t, scopes, "profile", "scope must contain profile")
}

func TestBegin_ExtraAuthParams(t *testing.T) {
	fi := oidctest.New()
	t.Cleanup(fi.Server.Close)
	p := newProvider(t, fi, oidckind.Options{
		ExtraAuthParams: map[string]string{"hd": "example.com"},
	})

	authURL, err := p.Begin(context.Background(), testState)
	require.NoError(t, err)

	u, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.Equal(t, "example.com", u.Query().Get("hd"), "hd extra param")
}

func TestBegin_ExtraScopes(t *testing.T) {
	fi := oidctest.New()
	t.Cleanup(fi.Server.Close)

	ctx := gooidc.ClientContext(context.Background(), fi.Client())
	p, err := oidckind.NewWithOptions(ctx, idp.Config{
		Issuer:       fi.URL(),
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		RedirectURL:  testRedirectURL,
		Scopes:       []string{"groups"},
	}, oidckind.Options{})
	require.NoError(t, err)

	authURL, err := p.Begin(context.Background(), testState)
	require.NoError(t, err)

	u, _ := url.Parse(authURL)
	scopes := strings.Split(u.Query().Get("scope"), " ")
	assert.Contains(t, scopes, "groups", "extra scope forwarded")
}

// ---- Complete tests ---------------------------------------------------------------

func TestComplete(t *testing.T) {
	cases := []struct {
		name          string
		setup         func(t *testing.T, fi *oidctest.FakeIssuer)
		cb            idp.CallbackParams
		opts          oidckind.Options
		wantErr       bool
		wantErrSub    string
		wantPrincipal func(t *testing.T, got identity.Principal)
	}{
		{
			name: "happy verified email → IdPUser verified",
			setup: func(_ *testing.T, fi *oidctest.FakeIssuer) {
				fi.SetClaims(map[string]any{
					"email":          "alice@example.com",
					"email_verified": true,
					"name":           "Alice",
				})
			},
			cb: idp.CallbackParams{Code: testState, State: testState},
			wantPrincipal: func(t *testing.T, got identity.Principal) {
				t.Helper()
				assert.Equal(t, "alice@example.com", got.Email().String())
				assert.True(t, got.EmailVerified())
				assert.Equal(t, "Alice", got.DisplayName())
				assert.Equal(t, identity.KindIdP, got.Kind())
			},
		},
		{
			name: "email_verified=false → Principal unverified, no error",
			setup: func(_ *testing.T, fi *oidctest.FakeIssuer) {
				fi.SetClaims(map[string]any{
					"email":          "bob@example.com",
					"email_verified": false,
					"name":           "Bob",
				})
			},
			cb: idp.CallbackParams{Code: testState, State: testState},
			wantPrincipal: func(t *testing.T, got identity.Principal) {
				t.Helper()
				assert.Equal(t, "bob@example.com", got.Email().String())
				assert.False(t, got.EmailVerified())
				assert.Equal(t, identity.KindIdP, got.Kind())
			},
		},
		{
			name: `email_verified as string "true" (Google quirk) → verified`,
			setup: func(_ *testing.T, fi *oidctest.FakeIssuer) {
				// Google historically sent "true" as a string
				fi.ClaimsFunc = func(clientID, nonce string) map[string]any {
					return map[string]any{
						"email":          "carol@example.com",
						"email_verified": "true",
						"name":           "Carol",
					}
				}
			},
			cb: idp.CallbackParams{Code: testState, State: testState},
			wantPrincipal: func(t *testing.T, got identity.Principal) {
				t.Helper()
				assert.Equal(t, "carol@example.com", got.Email().String())
				assert.True(t, got.EmailVerified())
			},
		},
		{
			name: "nonce mismatch → error",
			setup: func(_ *testing.T, fi *oidctest.FakeIssuer) {
				fi.ClaimsFunc = func(clientID, nonce string) map[string]any {
					return map[string]any{
						"email":          "alice@example.com",
						"email_verified": true,
						"name":           "Alice",
						"nonce":          "WRONG-NONCE", // deliberately different
					}
				}
			},
			cb:         idp.CallbackParams{Code: testState, State: testState},
			wantErr:    true,
			wantErrSub: "nonce mismatch",
		},
		{
			name: "expired id_token → error",
			setup: func(_ *testing.T, fi *oidctest.FakeIssuer) {
				fi.ClaimsFunc = func(clientID, nonce string) map[string]any {
					return map[string]any{
						"email":          "alice@example.com",
						"email_verified": true,
						"name":           "Alice",
						"exp":            oidctest.PastTime(),
						"nonce":          nonce,
					}
				}
			},
			cb:         idp.CallbackParams{Code: testState, State: testState},
			wantErr:    true,
			wantErrSub: "id_token verify",
		},
		{
			name: "audience mismatch → error",
			setup: func(_ *testing.T, fi *oidctest.FakeIssuer) {
				fi.ClaimsFunc = func(clientID, nonce string) map[string]any {
					return map[string]any{
						"email":          "alice@example.com",
						"email_verified": true,
						"name":           "Alice",
						"aud":            "other-client", // wrong audience
						"nonce":          nonce,
					}
				}
			},
			cb:         idp.CallbackParams{Code: testState, State: testState},
			wantErr:    true,
			wantErrSub: "id_token verify",
		},
		{
			name: "missing email claim → error",
			setup: func(_ *testing.T, fi *oidctest.FakeIssuer) {
				fi.ClaimsFunc = func(clientID, nonce string) map[string]any {
					return map[string]any{
						"name":  "No Email",
						"nonce": nonce,
					}
				}
			},
			cb:         idp.CallbackParams{Code: testState, State: testState},
			wantErr:    true,
			wantErrSub: "no email claim",
		},
		{
			name:       "IdP-reported error → error, no token fetch",
			setup:      func(_ *testing.T, fi *oidctest.FakeIssuer) {}, // no token called
			cb:         idp.CallbackParams{Error: "access_denied", State: testState},
			wantErr:    true,
			wantErrSub: "access_denied",
		},
		{
			name: "missing id_token in token response → error",
			setup: func(_ *testing.T, fi *oidctest.FakeIssuer) {
				fi.SuppressIDToken()
			},
			cb:         idp.CallbackParams{Code: testState, State: testState},
			wantErr:    true,
			wantErrSub: "missing id_token",
		},
		{
			name: "id_token signed by wrong RSA key → signature verification error",
			setup: func(t *testing.T, fi *oidctest.FakeIssuer) {
				t.Helper()
				// Generate a second RSA key not registered in the issuer's JWKS.
				wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
				require.NoError(t, err, "test setup: rsa.GenerateKey")
				fi.TokenOverride = func(clientID, nonce string) string {
					// Build valid claims (correct iss/aud/exp/nonce/email) but
					// sign with the key that is absent from the JWKS endpoint.
					claims := map[string]any{
						"iss":            fi.URL(),
						"aud":            clientID,
						"iat":            time.Now().Unix(),
						"exp":            oidctest.FutureTime(),
						"nonce":          nonce,
						"sub":            "test-sub",
						"email":          "alice@example.com",
						"email_verified": true,
						"name":           "Alice",
					}
					return oidctest.SignTokenWithKey(wrongKey, claims)
				}
			},
			cb:         idp.CallbackParams{Code: testState, State: testState},
			wantErr:    true,
			wantErrSub: "failed to verify signature",
		},
		{
			name: "VerifyClaims hook returning error → Complete fails",
			setup: func(_ *testing.T, fi *oidctest.FakeIssuer) {
				fi.SetClaims(map[string]any{
					"email":          "alice@example.com",
					"email_verified": true,
					"name":           "Alice",
					"hd":             "wrong.com",
				})
			},
			cb: idp.CallbackParams{Code: testState, State: testState},
			opts: oidckind.Options{
				VerifyClaims: func(claims map[string]any) error {
					hd, _ := claims["hd"].(string)
					if hd != "example.com" {
						return fmt.Errorf("hd mismatch: got %q", hd)
					}
					return nil
				},
			},
			wantErr:    true,
			wantErrSub: "provider claim check",
		},
		{
			name: "VerifyClaims hook receives raw claims including custom",
			setup: func(_ *testing.T, fi *oidctest.FakeIssuer) {
				fi.SetClaims(map[string]any{
					"email":          "alice@example.com",
					"email_verified": true,
					"name":           "Alice",
					"custom_field":   "custom_value",
				})
			},
			cb: idp.CallbackParams{Code: testState, State: testState},
			opts: oidckind.Options{
				VerifyClaims: func(claims map[string]any) error {
					if claims["custom_field"] != "custom_value" {
						return fmt.Errorf("custom_field not received in VerifyClaims")
					}
					return nil
				},
			},
			wantPrincipal: func(t *testing.T, got identity.Principal) {
				t.Helper()
				assert.Equal(t, "alice@example.com", got.Email().String())
				assert.True(t, got.EmailVerified())
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fi := oidctest.New()
			t.Cleanup(fi.Server.Close)

			tc.setup(t, fi)

			p := newProvider(t, fi, tc.opts)

			got, _, err := completeWith(t, fi, p, tc.cb)

			if tc.wantErr {
				require.Error(t, err, "expected error for case %q", tc.name)
				if tc.wantErrSub != "" {
					assert.Contains(t, err.Error(), tc.wantErrSub)
				}
				return
			}

			require.NoError(t, err)
			if tc.wantPrincipal != nil {
				tc.wantPrincipal(t, got)
			}
		})
	}
}

// newFederationProvider creates an oidc provider with Federation=true backed
// by the fake issuer.
func newFederationProvider(t *testing.T, fi *oidctest.FakeIssuer) idp.Provider {
	t.Helper()
	ctx := gooidc.ClientContext(context.Background(), fi.Client())
	p, err := oidckind.NewWithOptions(ctx, idp.Config{
		Issuer:       fi.URL(),
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		RedirectURL:  testRedirectURL,
		Federation:   true,
	}, oidckind.Options{})
	require.NoError(t, err, "NewWithOptions with Federation must succeed")
	return p
}

// TestFederation_OfflineAccessInScope asserts that when Federation=true the
// authorize URL includes "offline_access" in the scope parameter.
func TestFederation_OfflineAccessInScope(t *testing.T) {
	fi := oidctest.New()
	t.Cleanup(fi.Server.Close)
	p := newFederationProvider(t, fi)

	authURL, err := p.Begin(context.Background(), testState)
	require.NoError(t, err)

	u, err := url.Parse(authURL)
	require.NoError(t, err)

	scopes := strings.Split(u.Query().Get("scope"), " ")
	assert.Contains(t, scopes, "offline_access", "federation=true must request offline_access scope")
}

// TestFederation_TokenSetPopulatedWhenRefreshTokenPresent asserts that
// Complete returns a non-nil TokenSet when the IdP returns a refresh_token
// and Federation=true.
func TestFederation_TokenSetPopulatedWhenRefreshTokenPresent(t *testing.T) {
	fi := oidctest.New()
	t.Cleanup(fi.Server.Close)
	fi.SetClaims(map[string]any{
		"email":          "alice@example.com",
		"email_verified": true,
		"name":           "Alice",
	})
	fi.SetRefreshToken("fake-refresh-token-xyz")
	p := newFederationProvider(t, fi)

	_, ts, err := completeWith(t, fi, p, idp.CallbackParams{Code: testState, State: testState})
	require.NoError(t, err)
	require.NotNil(t, ts, "TokenSet must be non-nil when refresh_token is present and Federation=true")
	assert.Equal(t, "fake-refresh-token-xyz", ts.RefreshToken)
	assert.Equal(t, "x", ts.AccessToken)
	assert.Equal(t, testClientID, ts.ClientID)
	assert.Equal(t, testClientSecret, ts.ClientSecret)
	assert.NotEmpty(t, ts.TokenEndpoint, "TokenEndpoint must be populated from discovery")
	assert.Contains(t, ts.Scope, "offline_access", "Scope must include offline_access")
	assert.False(t, ts.ExpiresAt.IsZero(), "ExpiresAt must be set")
}

// TestFederation_TokenSetNilWhenNoRefreshToken asserts that Complete returns
// nil TokenSet when Federation=true but the IdP does not return a
// refresh_token (e.g. offline_access refused by IdP policy).
func TestFederation_TokenSetNilWhenNoRefreshToken(t *testing.T) {
	fi := oidctest.New()
	t.Cleanup(fi.Server.Close)
	fi.SetClaims(map[string]any{
		"email":          "alice@example.com",
		"email_verified": true,
		"name":           "Alice",
	})
	// No refresh token configured — IdP did not include one in the response.
	p := newFederationProvider(t, fi)

	_, ts, err := completeWith(t, fi, p, idp.CallbackParams{Code: testState, State: testState})
	require.NoError(t, err)
	assert.Nil(t, ts, "TokenSet must be nil when no refresh_token in response")
}

// TestFederation_TokenSetNilWhenFederationDisabled asserts that Complete
// returns nil TokenSet even if the IdP returns a refresh_token when
// Federation=false (the refresh token is not captured in the non-federation
// flow).
func TestFederation_TokenSetNilWhenFederationDisabled(t *testing.T) {
	fi := oidctest.New()
	t.Cleanup(fi.Server.Close)
	fi.SetClaims(map[string]any{
		"email":          "alice@example.com",
		"email_verified": true,
		"name":           "Alice",
	})
	fi.SetRefreshToken("unexpected-refresh-token")
	// Create provider WITHOUT Federation.
	p := newProvider(t, fi, oidckind.Options{})

	_, ts, err := completeWith(t, fi, p, idp.CallbackParams{Code: testState, State: testState})
	require.NoError(t, err)
	assert.Nil(t, ts, "TokenSet must be nil when Federation=false")
}

// TestNewWithOptions_NoInjectedClient_SSRFGuarded proves the production path
// (no client in the context) routes discovery through the SSRF-guarded
// safehttp client: the fake issuer is a loopback httptest server, which the
// guard refuses. The injection-based tests above (which DO supply a loopback
// client via ClientContext) confirm the guard is bypassed only when a caller
// deliberately provides its own client.
func TestNewWithOptions_NoInjectedClient_SSRFGuarded(t *testing.T) {
	fi := oidctest.New()
	t.Cleanup(fi.Server.Close)

	_, err := oidckind.NewWithOptions(context.Background(), idp.Config{
		Issuer:       fi.URL(), // http://127.0.0.1:<port> — loopback
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		RedirectURL:  testRedirectURL,
	}, oidckind.Options{})

	require.Error(t, err, "discovery to a loopback issuer must be refused by the SSRF guard")
	assert.Contains(t, err.Error(), "blocked", "error should come from the safehttp guard")
}

// TestOIDCKindRegistered verifies the init() registered the kind.
func TestOIDCKindRegistered(t *testing.T) {
	k, ok := registry.Get("oidc")
	require.True(t, ok, "oidc kind must be registered")
	assert.Equal(t, "oidc", k.Name())
}

// TestWizardKindReturnsWizard verifies the kind returns a non-nil Wizard.
func TestWizardKindReturnsWizard(t *testing.T) {
	k, ok := registry.Get("oidc")
	require.True(t, ok)
	w := k.Wizard()
	require.NotNil(t, w, "Wizard() must return non-nil")
}

// TestValidateSpec covers the oidc kind's contribution to the
// ClusterIdentityProvider validity controller's kind-specific spec gate.
func TestValidateSpec(t *testing.T) {
	k := &oidckind.Kind{}
	t.Run("issuer set: valid", func(t *testing.T) {
		msg := k.ValidateSpec(spiceboxv1alpha1.ClusterIdentityProviderSpec{
			Kind: "oidc", Issuer: "https://issuer.example.com",
		})
		assert.Empty(t, msg)
	})
	t.Run("issuer empty: invalid", func(t *testing.T) {
		msg := k.ValidateSpec(spiceboxv1alpha1.ClusterIdentityProviderSpec{Kind: "oidc"})
		assert.Contains(t, msg, "spec.issuer is required for kind=oidc")
	})
}

// TestDiscoveryURL verifies the discovery probe target is derived from
// spec.Issuer.
func TestDiscoveryURL(t *testing.T) {
	k := &oidckind.Kind{}
	got := k.DiscoveryURL(spiceboxv1alpha1.ClusterIdentityProviderSpec{
		Kind: "oidc", Issuer: "https://issuer.example.com",
	})
	assert.Equal(t, "https://issuer.example.com/.well-known/openid-configuration", got)
}

// TestAllowedNonLocal verifies oidc authenticates against a remote issuer,
// so it must be admissible on a non-local (real) cluster.
func TestAllowedNonLocal(t *testing.T) {
	k := &oidckind.Kind{}
	assert.True(t, k.AllowedNonLocal(), "oidc must be allowed on a non-local cluster")
}
