package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pemBytes PKCS1-PEM-encodes key, the same shape GitHub hands out for App
// private keys.
func pemBytes(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// wantOnlyPath returns an http.HandlerFunc that fails the test loudly if hit
// on any path other than want, then delegates to next. This is the
// distinct-per-path shape the fake needs: a fake that answers identically
// regardless of path would mask a bug where the minter hits the wrong
// endpoint, or hits an endpoint more than once.
func wantOnlyPath(t *testing.T, want string, next http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != want {
			t.Errorf("unexpected request to %s %s; only %s is a real GitHub endpoint here", r.Method, r.URL.Path, want)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		next(w, r)
	}
}

func TestMint_SignsAJWTAndExchangesItForAnInstallationToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "generate a test signing key")

	var gotAuth, gotPath, gotMethod string
	srv := httptest.NewServer(wantOnlyPath(t, "/app/installations/67890/access_tokens",
		func(w http.ResponseWriter, r *http.Request) {
			gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"ghs_demotoken","expires_at":"2026-08-19T12:00:00Z"}`))
		}))
	t.Cleanup(srv.Close)

	m := NewHTTPMinter(WithBaseURL(srv.URL))
	got, err := m.Mint(context.Background(), MintRequest{
		AppID: "12345", PrivateKeyPEM: pemBytes(t, key), InstallationID: "67890",
	})
	require.NoError(t, err, "Mint")

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/app/installations/67890/access_tokens", gotPath)
	assert.True(t, strings.HasPrefix(gotAuth, "Bearer "), "the App JWT goes in the Authorization header")
	assert.Equal(t, "ghs_demotoken", string(got.AccessToken.UnderlyingValue()))
	assert.Equal(t, 2026, got.ExpiresAt.Year())
	assert.False(t, got.ExpiresAt.IsZero(), "an installation token must always carry its expiry")
}

func TestMint_SurfacesWhichLegFailed(t *testing.T) {
	srv := httptest.NewServer(wantOnlyPath(t, "/app/installations/2/access_tokens",
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"A JSON web token could not be decoded"}`))
		}))
	t.Cleanup(srv.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	_, err = NewHTTPMinter(WithBaseURL(srv.URL)).Mint(context.Background(), MintRequest{
		AppID: "1", PrivateKeyPEM: pemBytes(t, key), InstallationID: "2",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "access_tokens", "the error must name the leg that failed")
	assert.NotContains(t, err.Error(), "PRIVATE KEY", "never echo key material into an error")
}

// TestMint_MalformedPEMIsRejectedBeforeAnyNetworkCall also carries the
// package's "never leak key material into an error" guard for the parse
// leg. That guard used to live in a separate test
// (TestMint_NeverLeaksActualKeyBytesIntoParseError) that truncated a
// well-formed PEM's footer and asserted the resulting error didn't contain
// any base64 body line. Mutation testing showed that test had no teeth: a
// truncated footer makes pem.Decode fail before ever reaching
// x509.ParsePKCS1PrivateKey, so it took the exact same
// jwt.ErrKeyMustBePEMEncoded branch — and byte-identical error string — as
// the trivial "not a pem" input below, despite the doc comment's claim of
// exercising "the exact same parse path" as a real corrupted key. Worse,
// the invariant it was guarding ("must not wrap") is structural, not
// textual: jwt's underlying error text does not meaningfully vary across
// malformed-PEM shapes, so no string a test could search for would ever
// discriminate a correct implementation from one that reintroduced the
// forbidden %w wrap. errors.Unwrap being nil is the only assertion that
// actually proves no wrap happened; the test below asserts that directly.
func TestMint_MalformedPEMIsRejectedBeforeAnyNetworkCall(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(srv.Close)

	_, err := NewHTTPMinter(WithBaseURL(srv.URL)).Mint(context.Background(), MintRequest{
		AppID: "1", PrivateKeyPEM: []byte("not a pem"), InstallationID: "2",
	})
	require.Error(t, err)
	assert.False(t, called, "a bad key must fail locally, not burn a request")
	assert.Nil(t, errors.Unwrap(err),
		"the PEM parse error must not be %w-wrapped: jwt's own error can echo the input it was given")
}

func TestMint_FailsClosedOnAnUnboundedOrUnparseableResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "response missing expires_at: error, not a zero-time success",
			body: `{"token":"ghs_demotoken"}`,
		},
		{
			name: "response missing token: error",
			body: `{"expires_at":"2026-08-19T12:00:00Z"}`,
		},
		{
			name: "response is not valid JSON: error naming the decode leg",
			body: `not json`,
		},
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "generate a test signing key")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(wantOnlyPath(t, "/app/installations/2/access_tokens",
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tc.body))
				}))
			t.Cleanup(srv.Close)

			got, err := NewHTTPMinter(WithBaseURL(srv.URL)).Mint(context.Background(), MintRequest{
				AppID: "1", PrivateKeyPEM: pemBytes(t, key), InstallationID: "2",
			})
			require.Error(t, err)
			assert.True(t, got.ExpiresAt.IsZero(), "a failed mint must not return a usable token")
			assert.Empty(t, got.AccessToken.UnderlyingValue())
		})
	}
}

func TestMint_StampsIssuedAtAndExpiryFromTheInjectedClock(t *testing.T) {
	fixed := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "generate a test signing key")

	var gotAuth string
	srv := httptest.NewServer(wantOnlyPath(t, "/app/installations/2/access_tokens",
		func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"ghs_demotoken","expires_at":"2026-08-19T12:00:00Z"}`))
		}))
	t.Cleanup(srv.Close)

	m := NewHTTPMinter(WithBaseURL(srv.URL), WithClock(func() time.Time { return fixed }))
	_, err = m.Mint(context.Background(), MintRequest{
		AppID: "42", PrivateKeyPEM: pemBytes(t, key), InstallationID: "2",
	})
	require.NoError(t, err, "Mint")

	require.True(t, strings.HasPrefix(gotAuth, "Bearer "))
	rawJWT := strings.TrimPrefix(gotAuth, "Bearer ")

	var claims jwt.RegisteredClaims
	_, err = jwt.ParseWithClaims(rawJWT, &claims, func(*jwt.Token) (any, error) {
		return &key.PublicKey, nil
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithTimeFunc(func() time.Time { return fixed }))
	require.NoError(t, err, "the sent JWT must verify against the App's own public key")

	assert.Equal(t, "42", claims.Issuer, "issuer must be the App id")
	assert.WithinDuration(t, fixed.Add(-jwtClockSkewTolerance), claims.IssuedAt.Time, time.Second)
	assert.WithinDuration(t, fixed.Add(jwtLifetime), claims.ExpiresAt.Time, time.Second)
	assert.LessOrEqual(t, claims.ExpiresAt.Sub(claims.IssuedAt.Time), 10*time.Minute,
		"GitHub rejects an App JWT whose lifetime exceeds 10 minutes")
}

// conditionMessageMaxLength is the cap metav1.Condition.Message carries in
// every shipped CRD (maxLength: 32768). It is what makes an unbounded error
// string a LIVENESS bug rather than a cosmetic one: this error is surfaced on
// a status condition, and a message over the cap fails the status write, so
// the reconcile errors, requeues, mints again and fails to write again. The
// visible symptom of that hot loop is not the upstream error at all.
const conditionMessageMaxLength = 32768

// TestMint_ErrorStaysWithinTheConditionMessageCap drives the exact failure
// this bounds: an upstream answering with a body far larger than the cap.
//
// A proxy or gateway in front of the API returning an HTML error page is the
// ordinary way this happens — it needs no hostile upstream — and the body read
// is bounded only by maxResponseBodyBytes (1 MiB), thirty-two times the cap.
func TestMint_ErrorStaysWithinTheConditionMessageCap(t *testing.T) {
	const marker = "UPSTREAM-BODY-MARKER"
	huge := marker + strings.Repeat("x", 4*conditionMessageMaxLength)

	srv := httptest.NewServer(wantOnlyPath(t, "/app/installations/2/access_tokens",
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(huge))
		}))
	t.Cleanup(srv.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	_, err = NewHTTPMinter(WithBaseURL(srv.URL)).Mint(context.Background(), MintRequest{
		AppID: "1", PrivateKeyPEM: pemBytes(t, key), InstallationID: "2",
	})
	require.Error(t, err)

	assert.Lessf(t, len(err.Error()), conditionMessageMaxLength,
		"the error is surfaced on a status condition capped at %d characters; over it the status write fails and the session hot-loops",
		conditionMessageMaxLength)
	assert.NotContains(t, err.Error(), marker,
		"the upstream response body must not be interpolated into the error at all")

	// The diagnosis must survive the bound: excluding the body is only correct
	// because the status code is what tells an operator 401 (bad App JWT or
	// clock skew) from 404 (wrong installation id) from 502 (upstream proxy).
	assert.Contains(t, err.Error(), "502", "the status code is the diagnosis that has to remain")
	assert.Contains(t, err.Error(), "access_tokens", "the error must still name the leg that failed")
}

// TestMint_ErrorExcludesEvenASmallResponseBody covers the ordinary-sized case
// too. Bounding only the huge one would leave "interpolate the body when it
// happens to be short" passing, and the secrecy half of the reason — this path
// handles App credentials — does not care about length.
func TestMint_ErrorExcludesEvenASmallResponseBody(t *testing.T) {
	const marker = "A JSON web token could not be decoded"
	srv := httptest.NewServer(wantOnlyPath(t, "/app/installations/2/access_tokens",
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"` + marker + `"}`))
		}))
	t.Cleanup(srv.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	_, err = NewHTTPMinter(WithBaseURL(srv.URL)).Mint(context.Background(), MintRequest{
		AppID: "1", PrivateKeyPEM: pemBytes(t, key), InstallationID: "2",
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), marker,
		"the response body is excluded regardless of size — see the sibling appprovision client")
	assert.Contains(t, err.Error(), "401")
}
