package installcmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestPlatformCanonicalForEmail(t *testing.T) {
	// grant-admin canonicalizes the email exactly like channelsd does at
	// the pipeline boundary — same encoding, same lowercasing.
	got, err := CanonicalForEmail("Alice@Example.com")
	require.NoError(t, err)
	want, err := identity.EmailReference("alice@example.com").Canonical()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestPlatformCanonicalForEmail_MatchesPasswordKindPrincipal pins the one
// invariant the desktop bundle's password-IdP setup depends on:
// configureHook grants platform-admin to CanonicalForEmail("admin@ap.local"),
// while the "password" idp.Kind authenticates logins as
// identity.VerifiedEmail("admin@ap.local", "Local Admin") (see
// pkg/platform/identity/idp/passwordkind — its defaultLocalIdentity constant is this
// same literal). Both constructors only feed the (lowercased) email into
// Principal.Canonical(), so the two MUST produce the identical canonical —
// if they ever diverged (e.g. one path started using EmailVerified, Kind,
// or a different case-folding), the grant would silently target a
// different SpiceDB subject than the one that logs in, and every admin
// console permission check would fail despite a successful login.
func TestPlatformCanonicalForEmail_MatchesPasswordKindPrincipal(t *testing.T) {
	const adminEmail = "admin@ap.local"
	got, err := CanonicalForEmail(adminEmail)
	require.NoError(t, err)
	want, err := identity.VerifiedEmail(adminEmail, "Local Admin").Canonical()
	require.NoError(t, err)
	assert.Equal(t, want, got, "grant-admin's canonical must match the password idp.Kind's login Principal canonical")
}

// failingDialer is a apspicedb.ClusterDialer that fails the test if invoked. Used to
// prove the env-complete path never reaches for the cluster.
func failingDialer(t *testing.T) apspicedb.ClusterDialer {
	t.Helper()
	return func(context.Context) (*spicedb.Client, func(), error) {
		t.Error("cluster dialer must not be called when SPICEDB_* env is complete")
		return nil, nil, errors.New("unexpected cluster dial")
	}
}

func TestNewAuthzClient_UsesEnvWhenComplete(t *testing.T) {
	t.Setenv(spicedb.EnvEndpoint, "127.0.0.1:50051")
	t.Setenv(spicedb.EnvToken, "test-token")
	t.Setenv(spicedb.EnvInsecure, "true")

	cl, cleanup, err := apspicedb.NewAuthzClient(context.Background(), failingDialer(t))
	require.NoError(t, err)
	require.NotNil(t, cl, "env-complete path must build a client")
	require.NotNil(t, cleanup, "cleanup must be non-nil so callers can defer it")
	t.Cleanup(cleanup)
}

func TestNewAuthzClient_FallsBackToClusterWhenEnvIncomplete(t *testing.T) {
	// Both unset — the reported failure mode (no env, kubeconfig present).
	t.Setenv(spicedb.EnvEndpoint, "")
	t.Setenv(spicedb.EnvToken, "")

	sentinel, err := spicedb.NewClient("127.0.0.1:1", "tok", true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sentinel.Close() })

	cleanupCalled := false
	cl, cleanup, err := apspicedb.NewAuthzClient(context.Background(), func(context.Context) (*spicedb.Client, func(), error) {
		return sentinel, func() { cleanupCalled = true }, nil
	})
	require.NoError(t, err)
	assert.Same(t, sentinel, cl, "must return the cluster dialer's client")
	require.NotNil(t, cleanup)
	cleanup()
	assert.True(t, cleanupCalled, "must return and run the cluster dialer's cleanup")
}

func TestNewAuthzClient_PartialEnvFallsBackToCluster(t *testing.T) {
	// Endpoint set but token missing must NOT use env — it would dial with an
	// empty token. Fall back to the cluster instead.
	t.Setenv(spicedb.EnvEndpoint, "127.0.0.1:50051")
	t.Setenv(spicedb.EnvToken, "")

	dialed := false
	_, cleanup, err := apspicedb.NewAuthzClient(context.Background(), func(context.Context) (*spicedb.Client, func(), error) {
		dialed = true
		return nil, func() {}, nil
	})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.True(t, dialed, "partial env (no token) must fall back to the cluster dialer")
}

func TestNewAuthzClient_ErrorsWhenEnvIncompleteAndNoClusterAccess(t *testing.T) {
	t.Setenv(spicedb.EnvEndpoint, "")
	t.Setenv(spicedb.EnvToken, "")
	_, _, err := apspicedb.NewAuthzClient(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), spicedb.EnvEndpoint,
		"error must name the env vars the user can set to override")
}

func TestPlatformCommandTree(t *testing.T) {
	root := newRoot(t)
	plat, _, err := root.Find([]string{"platform"})
	require.NoError(t, err, "oap platform must be registered")
	names := map[string]bool{}
	for _, c := range plat.Commands() {
		names[c.Name()] = true
	}
	assert.True(t, names["grant-admin"], "grant-admin subcommand")
	assert.True(t, names["revoke-admin"], "revoke-admin subcommand")
	assert.True(t, names["list-admins"], "list-admins subcommand")
}
