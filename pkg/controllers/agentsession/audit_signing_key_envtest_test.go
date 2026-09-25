//go:build integration

// pkg/controllers/agentsession/audit_signing_key_envtest_test.go
//
// Reconcile-level tests for the per-session Ed25519 audit signing key:
// minted into the per-session Secret on first reconcile (seed under
// "audit-signing-key"), with the public key + keyID anchored on
// AgentSession.status as the K8s-witnessed trust anchor; and re-ensured /
// status-backfilled on the update path for Secrets that lack it or whose
// status pubkey was lost. The runner pod's unconditional SubPath mount
// would otherwise hang in ContainerCreating.
package agentsession_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// pubB64FromSeedHex derives the std-base64 Ed25519 public key from a
// hex-encoded 32-byte seed (the form stored in the per-session Secret).
func pubB64FromSeedHex(t *testing.T, seedHex string) string {
	t.Helper()
	seed, err := hex.DecodeString(seedHex)
	require.NoError(t, err, "decode seed hex")
	require.Len(t, seed, ed25519.SeedSize, "seed is 32 bytes")
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	return base64.StdEncoding.EncodeToString(pub)
}

func TestReconcileMintsAuditSigningKey(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	ac := validClass("ask-ac")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	require.NoError(t, env.Client.Create(ctx, validSession("ask-s", "ask-ac")), "create AgentSession")
	reconcileToWork(t, ctx, r, "ask-s")

	secName := agentsession.MemoryTokenSecretName(&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "ask-s"}})
	var sec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: secName}, &sec),
		"per-session Secret must exist")
	seed := sec.Data["audit-signing-key"]
	require.NotEmpty(t, seed, "audit-signing-key minted on first reconcile")
	assert.Len(t, seed, 64, "32-byte Ed25519 seed, hex-encoded")

	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ask-s"}, &sess))
	require.NotEmpty(t, sess.Status.AuditPublicKey, "status.AuditPublicKey anchored")
	require.NotEmpty(t, sess.Status.AuditKeyID, "status.AuditKeyID anchored")

	pub, err := base64.StdEncoding.DecodeString(sess.Status.AuditPublicKey)
	require.NoError(t, err, "status.AuditPublicKey is std-base64")
	assert.Len(t, pub, ed25519.PublicKeySize, "public key is 32 bytes")

	// The status pubkey is the public half of the seed in the Secret.
	assert.Equal(t, pubB64FromSeedHex(t, string(seed)), sess.Status.AuditPublicKey,
		"status pubkey derives from the Secret seed")
}

// TestReconcileAuditKeyStatusBackfillDoesNotRemint covers case (b) of the
// update path: the Secret still holds the original seed but status lost its
// AuditPublicKey/AuditKeyID. The reconciler must re-derive the public key
// from the EXISTING seed (resume safety) — it must NOT mint a fresh seed,
// which would orphan every entry the runner already signed with the old key.
func TestReconcileAuditKeyStatusBackfillDoesNotRemint(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	ac := validClass("ask-ac3")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	require.NoError(t, env.Client.Create(ctx, validSession("ask-s3", "ask-ac3")), "create AgentSession")
	reconcileToWork(t, ctx, r, "ask-s3")

	// Capture the minted seed before clearing status.
	secName := agentsession.MemoryTokenSecretName(&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "ask-s3"}})
	var sec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: secName}, &sec))
	before := append([]byte(nil), sec.Data["audit-signing-key"]...)
	require.Len(t, before, 64, "32-byte Ed25519 seed, hex-encoded")

	// Clear ONLY the status pubkey/keyID — leave the seed in the Secret.
	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ask-s3"}, &sess))
	sess.Status.AuditPublicKey = ""
	sess.Status.AuditKeyID = ""
	require.NoError(t, env.Client.Status().Update(ctx, &sess), "clear status pubkey/keyID (seed intact)")

	reconcileToWork(t, ctx, r, "ask-s3")

	var after corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: secName}, &after))
	assert.Equal(t, before, after.Data["audit-signing-key"],
		"seed must be byte-identical — status backfill must not re-mint the key")

	var afterSess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ask-s3"}, &afterSess))
	require.NotEmpty(t, afterSess.Status.AuditPublicKey, "status.AuditPublicKey re-derived from the existing seed")
	require.NotEmpty(t, afterSess.Status.AuditKeyID, "status.AuditKeyID re-derived from the existing seed")

	pub, err := base64.StdEncoding.DecodeString(afterSess.Status.AuditPublicKey)
	require.NoError(t, err, "status.AuditPublicKey is std-base64")
	assert.Len(t, pub, ed25519.PublicKeySize, "re-derived public key is 32 bytes")
	assert.Equal(t, pubB64FromSeedHex(t, string(before)), afterSess.Status.AuditPublicKey,
		"re-derived status pubkey is the public half of the ORIGINAL seed")
}

func TestReconcileEnsuresAuditSigningKeyOnExistingSecret(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	ac := validClass("ask-ac2")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	require.NoError(t, env.Client.Create(ctx, validSession("ask-s2", "ask-ac2")), "create AgentSession")
	reconcileToWork(t, ctx, r, "ask-s2")

	// Simulate a pre-existing Secret + lost status: strip the seed, keep
	// everything else, and clear the status pubkey/keyID.
	secName := agentsession.MemoryTokenSecretName(&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "ask-s2"}})
	var sec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: secName}, &sec))
	priorToken := sec.Data["token"]
	delete(sec.Data, "audit-signing-key")
	require.NoError(t, env.Client.Update(ctx, &sec), "strip audit-signing-key (pre-existing Secret)")

	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ask-s2"}, &sess))
	sess.Status.AuditPublicKey = ""
	sess.Status.AuditKeyID = ""
	require.NoError(t, env.Client.Status().Update(ctx, &sess), "clear status pubkey/keyID")

	reconcileToWork(t, ctx, r, "ask-s2")

	var after corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: secName}, &after))
	seed := after.Data["audit-signing-key"]
	assert.Len(t, seed, 64, "seed re-ensured on the update path")
	assert.Equal(t, priorToken, after.Data["token"], "other Secret keys untouched")

	var afterSess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ask-s2"}, &afterSess))
	require.NotEmpty(t, afterSess.Status.AuditPublicKey, "status.AuditPublicKey backfilled")
	require.NotEmpty(t, afterSess.Status.AuditKeyID, "status.AuditKeyID backfilled")
	assert.Equal(t, pubB64FromSeedHex(t, string(seed)), afterSess.Status.AuditPublicKey,
		"backfilled status pubkey derives from the re-ensured seed")
}
