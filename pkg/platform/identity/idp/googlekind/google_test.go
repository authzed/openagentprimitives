package googlekind_test

import (
	"context"
	"net/url"
	"testing"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/googlekind"
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

// newGoogleProvider builds a provider using googleOptions against the fake
// issuer. This lets us test the hd logic without hitting the real Google.
func newGoogleProvider(t *testing.T, fi *oidctest.FakeIssuer, hint string) idp.Provider {
	t.Helper()
	ctx := gooidc.ClientContext(context.Background(), fi.Client())
	p, err := oidckind.NewWithOptions(ctx, idp.Config{
		Issuer:       fi.URL(),
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		RedirectURL:  testRedirectURL,
	}, googlekind.GoogleOptions(hint))
	require.NoError(t, err, "NewWithOptions must succeed")
	return p
}

// completeWith calls Complete using the fake issuer's test server client.
func completeWith(t *testing.T, fi *oidctest.FakeIssuer, p idp.Provider, cb idp.CallbackParams) error {
	t.Helper()
	ctx := gooidc.ClientContext(context.Background(), fi.Client())
	_, _, err := p.Complete(ctx, cb)
	return err
}

// TestNewRejectsNonEmptyIssuer verifies Kind.New rejects a non-empty issuer.
func TestNewRejectsNonEmptyIssuer(t *testing.T) {
	k := &googlekind.Kind{}
	_, err := k.New(context.Background(), idp.Config{
		Issuer:       "https://accounts.google.com",
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pinned")
}

// TestNameAndRegistration verifies the kind is named "google" and registered.
func TestNameAndRegistration(t *testing.T) {
	k := &googlekind.Kind{}
	assert.Equal(t, "google", k.Name())

	got, ok := registry.Get("google")
	require.True(t, ok, "google kind must be registered")
	assert.Equal(t, "google", got.Name())
}

// TestAuthorizeURLCarriesHDHint verifies hd= appears in the authorize URL
// when a hint is set.
func TestAuthorizeURLCarriesHDHint(t *testing.T) {
	fi := oidctest.New()
	t.Cleanup(fi.Server.Close)

	p := newGoogleProvider(t, fi, "example.com")
	authURL, err := p.Begin(context.Background(), testState)
	require.NoError(t, err)

	u, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.Equal(t, "example.com", u.Query().Get("hd"), "hd hint must appear in authorize URL")
}

// TestHDClaimMismatch verifies Complete fails when the hd claim doesn't match
// the hint.
func TestHDClaimMismatch(t *testing.T) {
	fi := oidctest.New()
	t.Cleanup(fi.Server.Close)
	fi.SetClaims(map[string]any{
		"email":          "alice@evil.example",
		"email_verified": true,
		"name":           "Alice",
		"hd":             "evil.example",
	})

	p := newGoogleProvider(t, fi, "example.com")
	err := completeWith(t, fi, p, idp.CallbackParams{Code: testState, State: testState})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hosted domain")
}

// TestHDClaimMatching verifies Complete succeeds when hd claim matches hint.
func TestHDClaimMatching(t *testing.T) {
	fi := oidctest.New()
	t.Cleanup(fi.Server.Close)
	fi.SetClaims(map[string]any{
		"email":          "alice@example.com",
		"email_verified": true,
		"name":           "Alice",
		"hd":             "example.com",
	})

	p := newGoogleProvider(t, fi, "example.com")
	err := completeWith(t, fi, p, idp.CallbackParams{Code: testState, State: testState})
	require.NoError(t, err)
}

// TestMissingHDClaimWithHintSet verifies Complete fails when hd claim is
// absent but a hint is configured (consumer accounts must not pass domain login).
func TestMissingHDClaimWithHintSet(t *testing.T) {
	fi := oidctest.New()
	t.Cleanup(fi.Server.Close)
	fi.SetClaims(map[string]any{
		"email":          "alice@example.com",
		"email_verified": true,
		"name":           "Alice",
		// no "hd" claim — consumer Google account
	})

	p := newGoogleProvider(t, fi, "example.com")
	err := completeWith(t, fi, p, idp.CallbackParams{Code: testState, State: testState})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hosted domain")
}

// TestNoHintNoHDParamNoClaimCheck verifies that with no hint, googleOptions
// produces zero Options (no hd param, no claim check) and Complete succeeds.
func TestNoHintNoHDParamNoClaimCheck(t *testing.T) {
	opts := googlekind.GoogleOptions("")
	assert.Nil(t, opts.ExtraAuthParams, "no hint → no ExtraAuthParams")
	assert.Nil(t, opts.VerifyClaims, "no hint → no VerifyClaims")

	fi := oidctest.New()
	t.Cleanup(fi.Server.Close)
	// No hd claim in token — should succeed with no hint.
	fi.SetClaims(map[string]any{
		"email":          "alice@example.com",
		"email_verified": true,
		"name":           "Alice",
	})

	p := newGoogleProvider(t, fi, "")
	authURL, err := p.Begin(context.Background(), testState)
	require.NoError(t, err)
	u, _ := url.Parse(authURL)
	assert.Empty(t, u.Query().Get("hd"), "no hint → no hd param in authorize URL")

	err = completeWith(t, fi, p, idp.CallbackParams{Code: testState, State: testState})
	require.NoError(t, err)
}

// TestValidateSpec verifies the google kind has no additional spec rule
// beyond the generic webhook check.
func TestValidateSpec(t *testing.T) {
	k := &googlekind.Kind{}
	msg := k.ValidateSpec(spiceboxv1alpha1.ClusterIdentityProviderSpec{Kind: "google"})
	assert.Empty(t, msg)
}

// TestDiscoveryURL verifies the discovery probe target is pinned to
// Google's issuer regardless of spec.Issuer.
func TestDiscoveryURL(t *testing.T) {
	k := &googlekind.Kind{}
	got := k.DiscoveryURL(spiceboxv1alpha1.ClusterIdentityProviderSpec{Kind: "google"})
	assert.Equal(t, googlekind.Issuer+"/.well-known/openid-configuration", got)
}

// TestAllowedNonLocal verifies google authenticates against Google's remote
// issuer, so it must be admissible on a non-local (real) cluster.
func TestAllowedNonLocal(t *testing.T) {
	k := &googlekind.Kind{}
	assert.True(t, k.AllowedNonLocal(), "google must be allowed on a non-local cluster")
}
