package identitycmd

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
)

func TestResolveTokenSource(t *testing.T) {
	dir := t.TempDir()

	goodEnvPath := filepath.Join(dir, "good.env")
	require.NoError(t, os.WriteFile(goodEnvPath, []byte("MY_TOKEN=supersecret\n"), 0o600), "write good env file")

	missingKeyEnvPath := filepath.Join(dir, "missing-key.env")
	require.NoError(t, os.WriteFile(missingKeyEnvPath, []byte("OTHER=value\n"), 0o600), "write missing-key env file")

	cases := []struct {
		name      string
		literal   string
		envVar    string
		envFile   string
		stdin     string
		stdinFlag bool
		wantErr   bool
		wantToken string
	}{
		{
			name:      "envfile with valid key: returns token",
			envFile:   goodEnvPath + ":MY_TOKEN",
			wantToken: "supersecret",
		},
		{
			name:    "envfile missing key: errors",
			envFile: missingKeyEnvPath + ":MISSING",
			wantErr: true,
		},
		{
			name:    "envfile missing file: errors",
			envFile: "/nonexistent/.env:FOO",
			wantErr: true,
		},
		{
			name:    "envfile bad spec without colon: errors",
			envFile: "nodotenv",
			wantErr: true,
		},
		{
			name:    "multiple sources provided: errors",
			literal: "lit",
			envVar:  "ENV",
			wantErr: true,
		},
		{
			name:    "no sources provided: errors",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTokenSource(tc.literal, tc.envVar, tc.envFile, tc.stdin, tc.stdinFlag)
			if tc.wantErr {
				assert.Error(t, err, "ResolveTokenSource should error")
				return
			}
			require.NoError(t, err, "ResolveTokenSource should succeed")
			assert.Equal(t, tc.wantToken, string(got), "resolved token")
		})
	}
}

// --- putTokenIntoIdentity end-to-end tests (client.Client-injectable core) ---

func newPutTokenScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, fn := range []func(*runtime.Scheme) error{corev1.AddToScheme, spiceboxv1alpha1.AddToScheme} {
		require.NoError(t, fn(s), "AddToScheme")
	}
	return s
}

// makePutTokenIdentity builds an AgentIdentity carrying one static credential
// (credName) whose SecretRef points at secretName/token.
func makePutTokenIdentity(ns, name, credName, secretName string) *spiceboxv1alpha1.AgentIdentity {
	return &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name:   credName,
				Type:   "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: "token"}},
			}},
		},
	}
}

// secretData reads the token value written behind the AgentIdentity's cred.
func secretData(t *testing.T, c client.Client, ns, secretName string) (string, bool) {
	t.Helper()
	var sec corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: secretName}, &sec); err != nil {
		return "", false
	}
	return string(sec.Data["token"]), true
}

// TestPutTokenIntoIdentity_HappyStoresUnverified: a credential that resolves to
// no provider has no verifier (Unsupported) → the token is stored quietly.
func TestPutTokenIntoIdentity_HappyStoresUnverified(t *testing.T) {
	scheme := newPutTokenScheme(t)
	ai := makePutTokenIdentity("ns", "my-bot", "mycred", "mycred-secret")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai).Build()

	out := &strings.Builder{}
	err := putTokenIntoIdentity(context.Background(), out, c, "ns", "my-bot", "mycred",
		[]byte("some-token-value"), false, strings.NewReader(""), false)
	require.NoError(t, err, out.String())
	assert.Contains(t, out.String(), "created Secret")
	got, ok := secretData(t, c, "ns", "mycred-secret")
	require.True(t, ok, "Secret should have been created")
	assert.Equal(t, "some-token-value", got)
}

// TestPutTokenIntoIdentity_FormatGateRejects proves the previously-missing
// format check now runs: a github-token credential (→ github-pat provider)
// with a wrong-shaped token is refused BEFORE any Secret write.
func TestPutTokenIntoIdentity_FormatGateRejects(t *testing.T) {
	scheme := newPutTokenScheme(t)
	ai := makePutTokenIdentity("ns", "my-bot", "github-token", "gh-secret")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai).Build()

	out := &strings.Builder{}
	err := putTokenIntoIdentity(context.Background(), out, c, "ns", "my-bot", "github-token",
		[]byte("sk-ant-not-a-github-token"), false, strings.NewReader(""), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to store credential")
	_, ok := secretData(t, c, "ns", "gh-secret")
	assert.False(t, ok, "malformed token must NOT be stored")
}

// TestPutTokenIntoIdentity_VerifyRejectedNonInteractiveFailsClosed: a
// format-valid github token that the provider rejects (401) fails closed on a
// non-interactive call and is not stored.
func TestPutTokenIntoIdentity_VerifyRejectedNonInteractiveFailsClosed(t *testing.T) {
	builtins.SetVerifyHTTPClient(func() *http.Client {
		return &http.Client{Transport: aptest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"message":"Bad credentials"}`)),
			}, nil
		})}
	})
	t.Cleanup(func() { builtins.SetVerifyHTTPClient(nil) })

	scheme := newPutTokenScheme(t)
	ai := makePutTokenIdentity("ns", "my-bot", "github-token", "gh-secret")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai).Build()

	out := &strings.Builder{}
	err := putTokenIntoIdentity(context.Background(), out, c, "ns", "my-bot", "github-token",
		[]byte("ghp_looksvalidbutrejected"), false, strings.NewReader(""), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--skip-verify", "non-interactive rejection must suggest --skip-verify")
	_, ok := secretData(t, c, "ns", "gh-secret")
	assert.False(t, ok, "provider-rejected token must NOT be stored non-interactively")
}

// TestPutTokenIntoIdentity_VerifyForbiddenNonInteractiveFailsClosed: a
// format-valid github token the provider ACCEPTS and then refuses this check
// for (403 — an SSO-restricted PAT looks exactly like this) is not stored on a
// non-interactive call. This is a credential-provisioning gate: with nobody
// there to ask, it fails closed.
//
// The wording assertion is the point of the test as much as the refusal is —
// this operator's credential works, and the message must not tell them
// otherwise. TestPutTokenIntoIdentity_SkipVerifyStores is the control that the
// same path does store when the check is not in the way.
func TestPutTokenIntoIdentity_VerifyForbiddenNonInteractiveFailsClosed(t *testing.T) {
	builtins.SetVerifyHTTPClient(func() *http.Client {
		return &http.Client{Transport: aptest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"message":"Resource protected by organization SAML enforcement."}`)),
			}, nil
		})}
	})
	t.Cleanup(func() { builtins.SetVerifyHTTPClient(nil) })

	scheme := newPutTokenScheme(t)
	ai := makePutTokenIdentity("ns", "my-bot", "github-token", "gh-secret")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai).Build()

	out := &strings.Builder{}
	err := putTokenIntoIdentity(context.Background(), out, c, "ns", "my-bot", "github-token",
		[]byte("ghp_liveButSSORestricted"), false, strings.NewReader(""), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--skip-verify", "a non-interactive refusal must name the override")
	assert.Contains(t, out.String(), "authenticated but was refused for this check",
		"the operator must be told their credential works and this check did not pass")
	assert.NotContains(t, out.String(), "token verification failed",
		"a refused check is not a failed verification; that wording sends people to replace a working credential")
	_, ok := secretData(t, c, "ns", "gh-secret")
	assert.False(t, ok, "nothing may be stored when the check could not be confirmed and nobody could be asked")
}

// TestPutTokenIntoIdentity_SkipVerifyStores: --skip-verify bypasses the live
// check (never the format gate) so a format-valid token stores without a probe.
func TestPutTokenIntoIdentity_SkipVerifyStores(t *testing.T) {
	scheme := newPutTokenScheme(t)
	ai := makePutTokenIdentity("ns", "my-bot", "github-token", "gh-secret")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai).Build()

	out := &strings.Builder{}
	err := putTokenIntoIdentity(context.Background(), out, c, "ns", "my-bot", "github-token",
		[]byte("ghp_validshape"), true /*skipVerify*/, strings.NewReader(""), false)
	require.NoError(t, err, out.String())
	assert.Contains(t, out.String(), "skipping live token verification")
	got, ok := secretData(t, c, "ns", "gh-secret")
	require.True(t, ok, "skip-verify should still store")
	assert.Equal(t, "ghp_validshape", got)
}

// TestPutTokenIntoIdentity_UpdatesExistingSecret covers the update (created=false)
// branch: an already-present Secret is overwritten, reported as "updated".
func TestPutTokenIntoIdentity_UpdatesExistingSecret(t *testing.T) {
	scheme := newPutTokenScheme(t)
	ai := makePutTokenIdentity("ns", "my-bot", "mycred", "mycred-secret")
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mycred-secret", Namespace: "ns"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte("old-value")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai, existing).Build()

	out := &strings.Builder{}
	err := putTokenIntoIdentity(context.Background(), out, c, "ns", "my-bot", "mycred",
		[]byte("new-value"), false, strings.NewReader(""), false)
	require.NoError(t, err, out.String())
	assert.Contains(t, out.String(), "updated Secret")
	got, ok := secretData(t, c, "ns", "mycred-secret")
	require.True(t, ok)
	assert.Equal(t, "new-value", got, "existing Secret should be overwritten")
}

// TestPutTokenIntoIdentity_StoreErrorSurfaces covers the store-failure path:
// a non-static credential can't be put-token'd, and the error is surfaced
// (not swallowed) after the gates pass.
func TestPutTokenIntoIdentity_StoreErrorSurfaces(t *testing.T) {
	scheme := newPutTokenScheme(t)
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "my-bot", Namespace: "ns"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{Name: "mycred", Type: "oauth"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai).Build()

	out := &strings.Builder{}
	err := putTokenIntoIdentity(context.Background(), out, c, "ns", "my-bot", "mycred",
		[]byte("some-token"), false, strings.NewReader(""), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only a single-key credential can be put-token'd")
	assert.NotContains(t, out.String(), "created Secret")
	assert.NotContains(t, out.String(), "updated Secret")
}

// TestPutTokenIntoIdentity_UnregisteredCredentialType_SurfacesRegistryError is
// the R15-shaped case: a non-static, non-oauth, non-federated type used to
// fall into the exact same "only static credentials" refusal as a well-formed
// oauth credential (both simply failed `cred.Type != "static"`). Routed
// through the registry, an unrecognized type fails at credkindregistry.Get
// itself and carries a DIFFERENT, more specific message — proof this path
// actually dispatches through the registry rather than a literal string
// comparison that happens to produce the same "refused" outcome either way.
func TestPutTokenIntoIdentity_UnregisteredCredentialType_SurfacesRegistryError(t *testing.T) {
	scheme := newPutTokenScheme(t)
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "my-bot", Namespace: "ns"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{Name: "mycred", Type: "nosuch"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai).Build()

	out := &strings.Builder{}
	err := putTokenIntoIdentity(context.Background(), out, c, "ns", "my-bot", "mycred",
		[]byte("some-token"), false, strings.NewReader(""), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown credential type",
		"an unregistered type must surface the registry's own diagnosis, not the generic static-only refusal")
	assert.NotContains(t, out.String(), "created Secret")
	assert.NotContains(t, out.String(), "updated Secret")
}
