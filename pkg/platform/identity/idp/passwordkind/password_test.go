package passwordkind_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/passwordkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
)

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := passwordkind.HashPassword(pw)
	require.NoError(t, err)
	return h
}

func TestKindRegistered(t *testing.T) {
	k, ok := registry.Get("password")
	require.True(t, ok, "password kind must be registered")
	assert.Equal(t, "password", k.Name())
}

func TestWizardReturnsNonNil(t *testing.T) {
	k, ok := registry.Get("password")
	require.True(t, ok)
	w := k.Wizard()
	require.NotNil(t, w, "Wizard() must return non-nil")
}

func TestNew_RejectsEmptyHash(t *testing.T) {
	k := &passwordkind.Kind{}
	_, err := k.New(context.Background(), idp.Config{ClientSecret: ""})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestNew_RejectsNonBcryptHash(t *testing.T) {
	k := &passwordkind.Kind{}
	_, err := k.New(context.Background(), idp.Config{ClientSecret: "plaintext-not-a-hash"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bcrypt")
}

func TestNew_RejectsMalformedBcryptPrefix(t *testing.T) {
	k := &passwordkind.Kind{}
	// Starts with "$2" but is not a well-formed bcrypt hash otherwise.
	_, err := k.New(context.Background(), idp.Config{ClientSecret: "$2not-a-real-hash"})
	require.Error(t, err)
}

func TestNew_AcceptsValidBcryptHash(t *testing.T) {
	k := &passwordkind.Kind{}
	p, err := k.New(context.Background(), idp.Config{ClientSecret: mustHash(t, "correct horse battery staple")})
	require.NoError(t, err)
	require.NotNil(t, p)
}

func TestBegin_ReturnsSameOriginPasswordLoginPath(t *testing.T) {
	k := &passwordkind.Kind{}
	p, err := k.New(context.Background(), idp.Config{ClientSecret: mustHash(t, "hunter2000")})
	require.NoError(t, err)

	redirect, err := p.Begin(context.Background(), "state-abc-123")
	require.NoError(t, err)

	u, err := url.Parse(redirect)
	require.NoError(t, err, "Begin must return a well-formed URL/path")
	assert.Equal(t, "/password/login", u.Path)
	assert.Equal(t, "state-abc-123", u.Query().Get("state"))
	assert.Empty(t, u.Host, "Begin must be a same-origin relative path, not an absolute URL")
}

func TestComplete_AlwaysErrors(t *testing.T) {
	k := &passwordkind.Kind{}
	p, err := k.New(context.Background(), idp.Config{ClientSecret: mustHash(t, "hunter2000")})
	require.NoError(t, err)

	_, ts, err := p.Complete(context.Background(), idp.CallbackParams{Code: "whatever"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/password/verify")
	assert.Nil(t, ts)
}

func TestVerifyPassword_MatchAndMismatch(t *testing.T) {
	k := &passwordkind.Kind{}
	p, err := k.New(context.Background(), idp.Config{
		ClientID:     "owner@example.com",
		ClientSecret: mustHash(t, "correct horse battery staple"),
	})
	require.NoError(t, err)

	verifier, ok := p.(idp.PasswordVerifier)
	require.True(t, ok, "password kind's Provider must implement idp.PasswordVerifier")

	principal, ok := verifier.VerifyPassword("correct horse battery staple")
	require.True(t, ok, "correct password must verify")
	assert.Equal(t, "owner@example.com", principal.Email().String())
	assert.True(t, principal.EmailVerified())

	_, ok = verifier.VerifyPassword("wrong password")
	assert.False(t, ok, "wrong password must not verify")
}

func TestVerifyPassword_PrincipalStableAcrossCalls(t *testing.T) {
	k := &passwordkind.Kind{}
	p, err := k.New(context.Background(), idp.Config{
		ClientID:     "owner@example.com",
		ClientSecret: mustHash(t, "hunter2000"),
	})
	require.NoError(t, err)
	verifier := p.(idp.PasswordVerifier)

	p1, ok1 := verifier.VerifyPassword("hunter2000")
	require.True(t, ok1)
	p2, ok2 := verifier.VerifyPassword("hunter2000")
	require.True(t, ok2)
	s1, err1 := p1.Subject()
	require.NoError(t, err1)
	s2, err2 := p2.Subject()
	require.NoError(t, err2)
	assert.Equal(t, s1, s2, "the same configured identity must yield a stable Subject across verifications")
}

func TestVerifyPassword_DefaultIdentityWhenClientIDEmpty(t *testing.T) {
	k := &passwordkind.Kind{}
	p, err := k.New(context.Background(), idp.Config{ClientSecret: mustHash(t, "hunter2000")})
	require.NoError(t, err)
	verifier := p.(idp.PasswordVerifier)

	principal, ok := verifier.VerifyPassword("hunter2000")
	require.True(t, ok)
	assert.Equal(t, "admin@ap.local", principal.Email().String(), "empty ClientID must fall back to the fixed default local identity")
}

// TestVerifyPassword_HashCostMismatchStillWorks proves VerifyPassword uses
// bcrypt's own comparison (which embeds the cost in the hash) rather than
// a fixed assumed cost — a hash produced at a different cost than the
// wizard's default must still verify correctly.
func TestVerifyPassword_HashCostMismatchStillWorks(t *testing.T) {
	h, err := bcrypt.GenerateFromPassword([]byte("hunter2000"), 4) // cheap cost, still valid
	require.NoError(t, err)

	k := &passwordkind.Kind{}
	p, err := k.New(context.Background(), idp.Config{ClientSecret: string(h)})
	require.NoError(t, err)
	verifier := p.(idp.PasswordVerifier)

	_, ok := verifier.VerifyPassword("hunter2000")
	assert.True(t, ok)
}

// TestValidateSpec covers the password kind's tightened rule: a password
// kind has no email-domain concept, so it must always run wide open.
func TestValidateSpec(t *testing.T) {
	k := &passwordkind.Kind{}
	t.Run("allowAnyEmail=true: valid", func(t *testing.T) {
		msg := k.ValidateSpec(spiceboxv1alpha1.ClusterIdentityProviderSpec{
			Kind: "password", AllowAnyEmail: true,
		})
		assert.Empty(t, msg)
	})
	t.Run("allowAnyEmail=false: invalid", func(t *testing.T) {
		msg := k.ValidateSpec(spiceboxv1alpha1.ClusterIdentityProviderSpec{
			Kind: "password", AllowAnyEmail: false,
		})
		assert.Contains(t, msg, "spec.allowAnyEmail must be true for kind=password")
	})
}

// TestDiscoveryURL verifies the password kind needs no remote discovery
// probe: it is local and non-federated.
func TestDiscoveryURL(t *testing.T) {
	k := &passwordkind.Kind{}
	got := k.DiscoveryURL(spiceboxv1alpha1.ClusterIdentityProviderSpec{Kind: "password"})
	assert.Empty(t, got)
}

// TestAllowedNonLocal verifies the password kind is local-only: no external
// issuer and no anti-brute-force posture means it must never be admitted on
// a non-local (real/public) cluster. The ClusterIdentityProvider validity
// controller enforces this via this method.
func TestAllowedNonLocal(t *testing.T) {
	k := &passwordkind.Kind{}
	assert.False(t, k.AllowedNonLocal(), "password must be local-only")
}
