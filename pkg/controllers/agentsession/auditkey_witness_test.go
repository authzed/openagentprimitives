package agentsession

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/auditkey"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/publisherkeys"
)

// operatorSigningMemory returns a memory facade that signs append-only writes as
// system:operator, plus a registry holding that publisher's key — the shape
// internal/cmd/operator hands the reconciler, and the trust set `oap audit verify`
// reads from the publisher-keys ConfigMap.
func operatorSigningMemory(t *testing.T) (memory.Memory, *publisherkeys.Registry) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 9
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)

	reg := publisherkeys.New()
	require.NoError(t, reg.Add("system:operator", provenance.KeyID(pub), pub))
	signer := provenance.NewSigner(priv, "system:operator")
	return provenance.NewSigningMemory(memory.NewLocal(inmem.NewBackend()), signer), reg
}

// sessionWithAuditKey returns a session carrying the audit key anchored on its
// status, at a fixed creationTimestamp.
func sessionWithAuditKey(t *testing.T, uid string) (*spiceboxv1alpha1.AgentSession, ed25519.PublicKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 1
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	s := incarnation(t, uid)
	s.CreationTimestamp = metav1.Unix(1770000000, 0)
	s.Status.AuditPublicKey = base64.StdEncoding.EncodeToString(pub)
	s.Status.AuditKeyID = provenance.KeyID(pub)
	return s, pub
}

// TestWitnessAuditKey_RecordsABindingAnOfflineVerifierCanTrust is the durable
// half of the trust root. The status field and the per-session Secret both die
// with the CR; the records the key signed are permanent and stay in a scope the
// next session of this name inherits. The witness is what stops those records
// reading as unknown-key once the CR is gone.
func TestWitnessAuditKey_RecordsABindingAnOfflineVerifierCanTrust(t *testing.T) {
	mem, componentKeys := operatorSigningMemory(t)
	r := &Reconciler{AuditKeyMemory: mem}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	sess, pub := sessionWithAuditKey(t, "uid-attempt-1")
	r.witnessAuditKey(ctx, sess)

	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	entries, err := auditkey.Entries(ctx, mem, scope)
	require.NoError(t, err)
	require.Len(t, entries, 1, "one record per key")

	require.NoError(t, provenance.VerifyEntrySignature(componentKeys, entries[0]),
		"the record is signed by the operator, whose key outlives every session")

	got, err := auditkey.Decode(entries[0])
	require.NoError(t, err)
	assert.Equal(t, "uid-attempt-1", got.SessionUID, "the binding names the instance that holds the private half")
	assert.Equal(t, sess.Status.AuditKeyID, got.KeyID)
	assert.Equal(t, base64.StdEncoding.EncodeToString(pub), got.PubKey)
}

// TestWitnessAuditKey_IsIdempotentAcrossReconciles: the record is written on
// every reconcile that has a key on status, so it MUST be a byte-identical
// re-put. A wall-clock createdAt would make the second one an append-only
// conflict, and the reconciler would log a failure forever.
func TestWitnessAuditKey_IsIdempotentAcrossReconciles(t *testing.T) {
	mem, _ := operatorSigningMemory(t)
	r := &Reconciler{AuditKeyMemory: mem}
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sess, _ := sessionWithAuditKey(t, "uid-attempt-1")

	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	r.witnessAuditKey(ctx, sess)
	r.witnessAuditKey(ctx, sess)
	r.witnessAuditKey(ctx, sess)

	entries, err := auditkey.Entries(ctx, mem, scope)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "three reconciles leave one record")

	// The re-put must have been accepted, not refused: Witness surfaces an error
	// on conflict, so ask it directly rather than inferring from the count.
	require.NoError(t, auditkey.Witness(ctx, mem, scope, string(sess.UID),
		sess.Status.AuditKeyID, sess.Status.AuditPublicKey, sess.CreationTimestamp.Time),
		"re-recording the same binding is accepted as an idempotent re-put")
}

// TestWitnessAuditKey_BothInstancesKeysSurviveInOneScope is the retry shape: the
// scope ends up holding the deleted instance's key alongside the live one, which
// is exactly what lets a continued chain verify end to end.
func TestWitnessAuditKey_BothInstancesKeysSurviveInOneScope(t *testing.T) {
	mem, _ := operatorSigningMemory(t)
	r := &Reconciler{AuditKeyMemory: mem}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	first, _ := sessionWithAuditKey(t, "uid-attempt-1")
	r.witnessAuditKey(ctx, first)

	// The redelivery: same name, new uid, a freshly minted key.
	second := incarnation(t, "uid-attempt-2")
	second.CreationTimestamp = metav1.Unix(1770000600, 0)
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 2
	}
	newPub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	second.Status.AuditPublicKey = base64.StdEncoding.EncodeToString(newPub)
	second.Status.AuditKeyID = provenance.KeyID(newPub)
	r.witnessAuditKey(ctx, second)

	entries, err := auditkey.Entries(ctx, mem, memory.Scope{Kind: "session", ID: first.Namespace + "/" + first.Name})
	require.NoError(t, err)
	require.Len(t, entries, 2, "the retired key's binding is kept alongside the live one")

	uids := map[string]string{}
	for _, e := range entries {
		c, decErr := auditkey.Decode(e)
		require.NoError(t, decErr)
		uids[c.SessionUID] = c.KeyID
	}
	assert.Equal(t, first.Status.AuditKeyID, uids["uid-attempt-1"])
	assert.Equal(t, second.Status.AuditKeyID, uids["uid-attempt-2"])
}
