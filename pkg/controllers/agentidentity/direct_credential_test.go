//go:build integration

// Vault-pinning behavior for type=directCredentials credentials (Phase B
// Task 5). The validity reconciler proves, via the directcred source's Verify,
// which vault a credential's token resolves to; it records that vault on
// status.pinnedVaults, set once per token. A later rotation re-verifies: a
// token that still resolves to the same vault updates the pin, one that
// resolves to a different vault (or fails to verify) turns the identity
// Valid=False/DirectCredentialInvalid rather than silently following the new
// vault.
//
// Testing seam (see the fake below): the real onepassword source registers
// itself only via the operator's blank imports, which a test binary never
// links — so this package's test binary registers a fakeOnePasswordSource
// under the SAME kind string with no collision. Each test asserts that fact as
// a precondition: if the real source were somehow linked, the Register in
// init() would have panicked and directcred.Get would return the real one.
package agentidentity_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentidentity"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"

	// Register the directCredentials credkind so validateSpecShape's
	// credkindregistry.ValidateCredential dispatch resolves in this test binary.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

// fakeOnePasswordSource stands in for the real 1Password source under the same
// kind string. Its Verify is driven per-test by verifyFn and counts calls, so a
// test can prove both the returned pin and that Verify was (or was NOT) hit.
type fakeOnePasswordSource struct {
	mu       sync.Mutex
	calls    int
	verifyFn func(cfg directcred.Config) (directcred.Pin, error)
}

var fakeOP = &fakeOnePasswordSource{}

func init() { directcred.Register(fakeOP) }

func (f *fakeOnePasswordSource) Kind() string { return directcred.SourceOnePassword }

func (f *fakeOnePasswordSource) Verify(_ context.Context, cfg directcred.Config) (directcred.Pin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.verifyFn == nil {
		return directcred.Pin{}, errors.New("fakeOnePasswordSource: no verifyFn configured")
	}
	return f.verifyFn(cfg)
}

// List/Fetch are unused by Task 5 (vault pinning is Verify-only).
func (f *fakeOnePasswordSource) List(context.Context, directcred.Config) ([]directcred.Ref, error) {
	return nil, errors.New("fakeOnePasswordSource: List not exercised by Task 5")
}

func (f *fakeOnePasswordSource) Fetch(context.Context, directcred.Config, string) (directcred.Credential, error) {
	return directcred.Credential{}, errors.New("fakeOnePasswordSource: Fetch not exercised by Task 5")
}

// configureFake resets the call counter and installs the per-test Verify
// behavior, and asserts the fake (not the real source) is what is registered.
func configureFake(t *testing.T, fn func(cfg directcred.Config) (directcred.Pin, error)) {
	t.Helper()
	src, ok := directcred.Get(directcred.SourceOnePassword)
	require.True(t, ok, "a %q source must be registered", directcred.SourceOnePassword)
	_, isFake := src.(*fakeOnePasswordSource)
	require.True(t, isFake,
		"the REAL onepassword source must not be linked into this test binary; got %T", src)

	fakeOP.mu.Lock()
	defer fakeOP.mu.Unlock()
	fakeOP.calls = 0
	fakeOP.verifyFn = fn
}

func fakeVerifyCalls() int {
	fakeOP.mu.Lock()
	defer fakeOP.mu.Unlock()
	return fakeOP.calls
}

func fp(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// directCredAI builds an AgentIdentity carrying one onepassword
// directCredentials credential pointing at tokenSecret, scoped to vault.
func directCredAI(name, tokenSecret, vault string) *spiceboxv1alpha1.AgentIdentity {
	return &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "logins", Type: "directCredentials",
				DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
					Source:    directcred.SourceOnePassword,
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: tokenSecret, Key: "token"},
					Vault:     vault,
				},
			}},
		},
	}
}

func directCredReconciler(env *testenv.Env) (*agentidentity.Reconciler, context.Context) {
	sr := adoptguard.NewSecretReader(env.Client, env.Client, adoptguard.Warn, func(types.NamespacedName) bool { return false })
	return &agentidentity.Reconciler{Client: env.Client, APIReader: env.Client, SecretReader: sr}, context.Background()
}

func validCond(t *testing.T, env *testenv.Env, name string) *metav1.Condition {
	t.Helper()
	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, env.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got))
	return meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid)
}

// TestDirectCredential_VaultPinnedSetOnce: a directCredentials/onepassword
// credential whose token Secret resolves is Valid=True and records one
// PinnedVaults entry carrying the fake's vault id and the token fingerprint. A
// SECOND reconcile with the unchanged token must neither re-verify (call
// counter stays 1) nor re-patch the object (resourceVersion unchanged).
func TestDirectCredential_VaultPinnedSetOnce(t *testing.T) {
	env := testenv.Shared(t)
	r, ctx := directCredReconciler(env)
	configureFake(t, func(directcred.Config) (directcred.Pin, error) {
		return directcred.Pin{VaultID: "vault-abc", VaultName: "demo-vault"}, nil
	})

	const token = "ops-token-v1"
	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "op-token-setonce", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte(token)},
	}))
	require.NoError(t, env.Client.Create(ctx, directCredAI("id-setonce", "op-token-setonce", "demo-vault")))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "id-setonce"}})
	require.NoError(t, err, "first reconcile")

	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "id-setonce"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a verified onepassword credential is Valid=True")
	assert.Equal(t, spiceboxv1alpha1.ReasonAllReferencesResolve, cond.Reason)

	require.Len(t, got.Status.PinnedVaults, 1, "exactly one vault pinned")
	assert.Equal(t, spiceboxv1alpha1.PinnedVault{
		Credential: "logins", VaultID: "vault-abc", VaultName: "demo-vault", TokenFingerprint: fp(token),
	}, got.Status.PinnedVaults[0])
	assert.Equal(t, 1, fakeVerifyCalls(), "Verify called once on first sight")

	rv := got.ResourceVersion

	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "id-setonce"}})
	require.NoError(t, err, "second reconcile")

	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "id-setonce"}, &got))
	assert.Equal(t, rv, got.ResourceVersion,
		"set-once: an unchanged token must not re-patch status (DeepEqual no-op)")
	assert.Equal(t, 1, fakeVerifyCalls(),
		"set-once: a matching token fingerprint must NOT re-hit the backend")
}

// TestDirectCredential_TokenRotation: after a pin is set, replacing the token
// Secret forces a re-verify. If the new token resolves to a different vault
// (Verify errors on the name mismatch) the identity goes
// Valid=False/DirectCredentialInvalid; if it still resolves to the same-named
// vault the pin updates to the new fingerprint and Valid stays True.
func TestDirectCredential_TokenRotation(t *testing.T) {
	cases := []struct {
		name       string
		rotatedFn  func(cfg directcred.Config) (directcred.Pin, error)
		wantStatus metav1.ConditionStatus
		wantReason string
		wantFP     string // expected PinnedVaults[0].TokenFingerprint after rotation ("" ⇒ pin unchanged)
	}{
		{
			name: "rotated token resolves to a DIFFERENT vault: Valid=False/DirectCredentialInvalid, pin preserved",
			rotatedFn: func(directcred.Config) (directcred.Pin, error) {
				return directcred.Pin{}, errors.New("onepassword: the accessible vault \"other-vault\" does not match the configured vault \"demo-vault\"")
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonAgentIdentityDirectCredentialInvalid,
			wantFP:     "", // the prior pin is kept on a verify failure
		},
		{
			name: "rotated token resolves to the SAME-named vault: pin updated, Valid stays True",
			rotatedFn: func(directcred.Config) (directcred.Pin, error) {
				return directcred.Pin{VaultID: "vault-abc", VaultName: "demo-vault"}, nil
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonAllReferencesResolve,
			wantFP:     fp("ops-token-v2"),
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.Shared(t)
			r, ctx := directCredReconciler(env)
			idName := "id-rotate-" + string(rune('a'+i))
			secName := "op-token-rotate-" + string(rune('a'+i))

			// First sight: pin vault-abc on the original token.
			configureFake(t, func(directcred.Config) (directcred.Pin, error) {
				return directcred.Pin{VaultID: "vault-abc", VaultName: "demo-vault"}, nil
			})
			require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secName, Namespace: "default"},
				Data:       map[string][]byte{"token": []byte("ops-token-v1")},
			}))
			require.NoError(t, env.Client.Create(ctx, directCredAI(idName, secName, "demo-vault")))
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: idName}})
			require.NoError(t, err, "first reconcile")
			require.Equal(t, fp("ops-token-v1"), pinFP(t, env, idName), "pin set on the original token")

			// Rotate the token and install the rotation outcome.
			configureFake(t, tc.rotatedFn)
			var sec corev1.Secret
			require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: secName}, &sec))
			sec.Data["token"] = []byte("ops-token-v2")
			require.NoError(t, env.Client.Update(ctx, &sec))

			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: idName}})
			require.NoError(t, err, "reconcile after rotation")
			assert.Positive(t, fakeVerifyCalls(), "a changed token fingerprint must re-verify")

			cond := validCond(t, env, idName)
			require.NotNil(t, cond)
			assert.Equal(t, tc.wantStatus, cond.Status)
			assert.Equal(t, tc.wantReason, cond.Reason)

			if tc.wantFP == "" {
				assert.Equal(t, fp("ops-token-v1"), pinFP(t, env, idName),
					"a failed re-verify must preserve the prior pin, not erase it")
			} else {
				assert.Equal(t, tc.wantFP, pinFP(t, env, idName), "a successful re-verify updates the pin's fingerprint")
			}
		})
	}
}

// pinFP returns the TokenFingerprint of the identity's single pinned vault, or
// "" when none is recorded.
func pinFP(t *testing.T, env *testenv.Env, name string) string {
	t.Helper()
	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, env.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got))
	if len(got.Status.PinnedVaults) == 0 {
		return ""
	}
	return got.Status.PinnedVaults[0].TokenFingerprint
}

// TestDirectCredential_UnregisteredSourceFailsClosed: a directCredentials
// credential whose source kind has NO registered directcred.Source must turn
// the identity Valid=False/DirectCredentialInvalid — never Valid=True with the
// vault-pin gate silently skipped. The CRD enum admits only known source
// names, so the live window for this is a binary missing its blank import
// (the gap pkg/platform/identity/directcred's enum_invariant_test.go guards);
// this test constructs that window honestly: THIS test binary registers only
// the fake "onepassword" (init above) and links neither real source package,
// so "secret" — a perfectly enum-valid source kind — is genuinely unregistered
// here, exercising exactly the branch an un-imported operator would hit.
func TestDirectCredential_UnregisteredSourceFailsClosed(t *testing.T) {
	env := testenv.Shared(t)
	r, ctx := directCredReconciler(env)

	// Precondition: "secret" must be unregistered in this binary, or this test
	// would silently exercise the registered path instead of the fail-closed one.
	_, registered := directcred.Get(directcred.SourceSecret)
	require.False(t, registered,
		"the real secretsource package must not be linked into this test binary; it is, so this test no longer exercises the unregistered branch")

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "login-secret-unreg", Namespace: "default"},
		Data: map[string][]byte{
			"a.example.com.username": []byte("demo-user"),
			"a.example.com.password": []byte("demo-pass"),
		},
	}))
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id-unreg-source", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "logins-secret", Type: "directCredentials",
				DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
					Source:    directcred.SourceSecret, // enum-valid, unregistered HERE
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "login-secret-unreg"},
				},
			}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ai))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "id-unreg-source"}})
	require.NoError(t, err, "reconcile (an unregistered source is a condition, not a transient error)")

	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "id-unreg-source"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"an identity must not report healthy while the vault-pin gate cannot run")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentIdentityDirectCredentialInvalid, cond.Reason)
	assert.Contains(t, cond.Message, "credentials[logins-secret]", "the message names the credential")
	assert.Contains(t, cond.Message, `"secret"`, "the message names the unregistered source kind")
	assert.Contains(t, cond.Message, "blank import", "the message points at the likely missing wiring")
	assert.Empty(t, got.Status.PinnedVaults, "nothing may be pinned by an unverifiable credential")
}

// TestDirectCredential_VerifyError: a Verify failure on first sight turns the
// identity Valid=False/DirectCredentialInvalid, carries the verify error's text
// in the condition message, and NEVER leaks the token bytes.
func TestDirectCredential_VerifyError(t *testing.T) {
	env := testenv.Shared(t)
	r, ctx := directCredReconciler(env)

	const token = "ops-token-secret-bytes"
	const verifyErr = "onepassword: expected exactly one accessible vault, found 2: [vault-a vault-b]"
	configureFake(t, func(directcred.Config) (directcred.Pin, error) {
		return directcred.Pin{}, errors.New(verifyErr)
	})

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "op-token-err", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte(token)},
	}))
	require.NoError(t, env.Client.Create(ctx, directCredAI("id-verifyerr", "op-token-err", "demo-vault")))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "id-verifyerr"}})
	require.NoError(t, err, "reconcile (a verify failure is a condition, not a transient error)")

	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "id-verifyerr"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentIdentityDirectCredentialInvalid, cond.Reason)
	assert.Contains(t, cond.Message, verifyErr, "the condition must carry the verify error text")
	assert.NotContains(t, cond.Message, token, "the token bytes must NEVER appear in a condition message")
	assert.Empty(t, got.Status.PinnedVaults, "a failed first-sight verify pins nothing")
}
