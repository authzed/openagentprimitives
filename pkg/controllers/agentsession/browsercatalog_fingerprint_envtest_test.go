//go:build integration

// pkg/controllers/agentsession/browsercatalog_fingerprint_envtest_test.go
//
// The one carry-forward trigger browsercatalog_envtest_test.go cannot reach:
// a CHANGED token fingerprint (trigger (c)). The vault-less `secret` source
// has no token key — its fingerprint hashes zero bytes and is constant — so
// those tests prove triggers (a) no-prior and (b) wake, but the
// fingerprint-change re-list branch needs a source whose credential carries a
// real token key. A fake source registered under the "onepassword" kind
// string provides it (the REAL onepassword package is linked only via the
// operator's blank imports, never into this test binary — asserted as a
// precondition, mirroring pkg/controllers/agentidentity's
// direct_credential_test.go), and doubles as a true List CALL COUNTER, so the
// cadence is pinned by counting rather than only by the changed-sentinel
// proxy.
package agentsession_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
)

// fakeBrowserOPSource stands in for the 1Password source under the same kind
// string. List returns one Ref whose ID encodes the token value it was called
// with, so the stamped catalog shows WHICH token version produced it, and a
// counter proves HOW OFTEN the reconciler listed. It also records the VaultID
// the reconciler passed, pinning the PinnedVaults → Config.VaultID plumbing.
type fakeBrowserOPSource struct {
	mu          sync.Mutex
	listCalls   int
	lastVaultID string
}

var fakeBrowserOP = &fakeBrowserOPSource{}

func init() { directcred.Register(fakeBrowserOP) }

func (f *fakeBrowserOPSource) Kind() string { return directcred.SourceOnePassword }

func (f *fakeBrowserOPSource) Verify(context.Context, directcred.Config) (directcred.Pin, error) {
	return directcred.Pin{VaultID: "vault-123", VaultName: "Engineering"}, nil
}

func (f *fakeBrowserOPSource) List(_ context.Context, cfg directcred.Config) ([]directcred.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	f.lastVaultID = cfg.VaultID
	tok := string(cfg.SecretData["token"])
	return []directcred.Ref{{
		ID:    "login-" + tok,
		Label: "Login (" + tok + ")",
		Hosts: []directcred.Host{{Name: "vaultapp.example.com"}},
	}}, nil
}

func (f *fakeBrowserOPSource) Fetch(context.Context, directcred.Config, string) (directcred.Credential, error) {
	return directcred.Credential{}, nil // never called by the AgentSession reconciler
}

func (f *fakeBrowserOPSource) snapshot() (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls, f.lastVaultID
}

// TestBrowserCatalogRelistsOnTokenFingerprintChange pins carry-forward trigger
// (c): a rotated token Secret — no wake — re-lists the catalog, and nothing
// else does. The call counter makes the cadence claim direct: exactly one List
// per distinct (start, fingerprint-change) cause across many reconciles.
func TestBrowserCatalogRelistsOnTokenFingerprintChange(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newBrowserReconciler(t, env)

	// Precondition (mirrors the agentidentity fake's): the registered
	// "onepassword" source must be THIS file's fake, not the real one.
	src, ok := directcred.Get(directcred.SourceOnePassword)
	require.True(t, ok, "an %q source must be registered", directcred.SourceOnePassword)
	_, isFake := src.(*fakeBrowserOPSource)
	require.True(t, isFake, "the REAL onepassword source must not be linked into this test binary; got %T", src)

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "btb-fp-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("tok-v1")},
	}
	require.NoError(t, env.Client.Create(ctx, tokenSecret), "create token Secret")

	id := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "btb-fp-id", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "logins", Type: "directCredentials",
				DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
					Source:    directcred.SourceOnePassword,
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "btb-fp-token", Key: "token"},
					Vault:     "Engineering",
				},
			}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, id), "create AgentIdentity")
	// Pin a vault on status so the reconciler's Config.VaultID plumbing is
	// observable at the fake.
	id.Status.PinnedVaults = []spiceboxv1alpha1.PinnedVault{{
		Credential: "logins", VaultID: "vault-123", VaultName: "Engineering",
		TokenFingerprint: agentsession.SecretTokenHash([]byte("tok-v1")),
	}}
	require.NoError(t, env.Client.Status().Update(ctx, id), "pin vault on identity status")

	require.NoError(t, env.Client.Create(ctx, browserToolboxBoundTo("btb-fp", "btb-fp-id")), "create BrowserToolbox")
	ac := classWithBrowsers("ac-browser-fp",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-fp"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	before, _ := fakeBrowserOP.snapshot()

	sess := validSession("s-browser-fp", "ac-browser-fp")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	key := types.NamespacedName{Namespace: "default", Name: "s-browser-fp"}
	reconcileToWork(t, ctx, r, "s-browser-fp")

	// Trigger (a), once: reconcileToWork drove FOUR reconciles, and only the
	// first to reach the browser block may list — the rest carry forward.
	afterStart, vaultID := fakeBrowserOP.snapshot()
	assert.Equal(t, before+1, afterStart, "exactly ONE List across the whole session start (carry-forward on every later pass)")
	assert.Equal(t, "vault-123", vaultID, "the pinned VaultID must reach the source's Config")

	rt := firstBrowser(t, ctx, env, "s-browser-fp")
	require.Len(t, rt.Catalog, 1)
	assert.Equal(t, "login-tok-v1", rt.Catalog[0].ID, "catalog listed against token v1")
	assert.Equal(t, agentsession.SecretTokenHash([]byte("tok-v1")), rt.CatalogFingerprint,
		"fingerprint is the digest of the token value listed against")

	// Steady state: more reconciles, still no List.
	for i := 0; i < 5; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}
	afterSteady, _ := fakeBrowserOP.snapshot()
	assert.Equal(t, afterStart, afterSteady, "steady-state reconciles must not re-list")

	// Trigger (c): rotate the token — NO wake. The changed fingerprint alone
	// must force exactly one re-list, and the catalog + fingerprint update.
	var liveTok corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "btb-fp-token"}, &liveTok))
	liveTok.Data["token"] = []byte("tok-v2")
	require.NoError(t, env.Client.Update(ctx, &liveTok), "rotate the token Secret (no wake)")

	reconcileToWork(t, ctx, r, "s-browser-fp")
	afterRotate, _ := fakeBrowserOP.snapshot()
	assert.Equal(t, afterSteady+1, afterRotate, "a rotated token must force exactly ONE re-list (trigger (c)), without a wake")

	rotated := firstBrowser(t, ctx, env, "s-browser-fp")
	require.Len(t, rotated.Catalog, 1)
	assert.Equal(t, "login-tok-v2", rotated.Catalog[0].ID, "catalog re-listed against token v2")
	assert.Equal(t, agentsession.SecretTokenHash([]byte("tok-v2")), rotated.CatalogFingerprint,
		"fingerprint updated to the rotated token's digest")

	// And back to carry-forward against the new fingerprint.
	for i := 0; i < 5; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}
	afterSettle, _ := fakeBrowserOP.snapshot()
	assert.Equal(t, afterRotate, afterSettle, "the new fingerprint carries forward; no further re-list")
}
