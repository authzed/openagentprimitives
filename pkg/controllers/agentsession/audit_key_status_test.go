// pkg/controllers/agentsession/audit_key_status_test.go
//
// status.auditPublicKey / auditKeyID are the K8s-witnessed trust root for a
// session's append-only audit chain: the operator registers them as a
// verify-on-write key for this session's publisher, and `oap audit verify`
// rebuilds its offline registry from them. The per-session Secret's
// "audit-signing-key" seed is the source of truth; status is a projection.
//
// A session runner holds `patch` on its own agentsessions/status, so it can
// write those fields. Backfilling status only when it is EMPTY let a
// runner-written value persist for the object's lifetime — the runner could
// sign honest entries under the real key, then rotate the anchor to one of its
// own, leaving the genuine prefix failing verification while its forgeries
// verify. Re-deriving from the seed on every reconcile makes that self-healing.
package agentsession

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// seedAndPub returns a deterministic hex seed and its std-base64 public half.
func seedAndPub(t *testing.T, fill byte) (seedHex, pubB64 string) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = fill
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	return hex.EncodeToString(seed), base64.StdEncoding.EncodeToString(pub)
}

func TestSyncAuditKeyStatus(t *testing.T) {
	realSeed, realPub := seedAndPub(t, 0x01)
	_, attackerPub := seedAndPub(t, 0x02)
	require.NotEqual(t, realPub, attackerPub, "fixture sanity: the two keys differ")

	cases := []struct {
		name        string
		status      spiceboxv1alpha1.AgentSessionStatus
		wantChanged bool
	}{
		{
			// The defect. A compromised runner patches its own /status with a
			// key it holds the private half of; the operator would go on
			// registering it as this session's trusted verify key forever.
			name:        "status carries a key that is not the seed's: overwritten from the Secret",
			status:      spiceboxv1alpha1.AgentSessionStatus{AuditPublicKey: attackerPub, AuditKeyID: "forged-key-id"},
			wantChanged: true,
		},
		{
			name:        "status empty (lost or pre-upgrade): backfilled from the seed",
			status:      spiceboxv1alpha1.AgentSessionStatus{},
			wantChanged: true,
		},
		{
			// A healthy session converges on the first pass; every later
			// reconcile must be a no-op, or status churns on every loop.
			name:        "status already matches the seed: no change reported",
			status:      spiceboxv1alpha1.AgentSessionStatus{AuditPublicKey: realPub, AuditKeyID: auditKeyIDForTest(t, realSeed)},
			wantChanged: false,
		},
		{
			// Half-written status (the pubkey survived, the keyID did not) is
			// still a disagreement and must be repaired, not accepted.
			name:        "pubkey matches but keyID does not: repaired",
			status:      spiceboxv1alpha1.AgentSessionStatus{AuditPublicKey: realPub, AuditKeyID: "stale"},
			wantChanged: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &spiceboxv1alpha1.AgentSession{Status: tc.status}

			changed, err := syncAuditKeyStatus(sess, realSeed)
			require.NoError(t, err)

			assert.Equal(t, tc.wantChanged, changed)
			assert.Equal(t, realPub, sess.Status.AuditPublicKey,
				"status must end up holding the public half of the Secret's seed, whatever it held before")
			assert.Equal(t, auditKeyIDForTest(t, realSeed), sess.Status.AuditKeyID,
				"keyID must name the seed-derived key")
		})
	}
}

// TestSyncAuditKeyStatusRejectsAnUnusableSeed pins the fail-loud half: a
// corrupt seed must surface as an error, never as a silently-kept status value.
func TestSyncAuditKeyStatusRejectsAnUnusableSeed(t *testing.T) {
	_, attackerPub := seedAndPub(t, 0x02)
	for _, seed := range []string{"", "zzzz", "0102"} {
		sess := &spiceboxv1alpha1.AgentSession{
			Status: spiceboxv1alpha1.AgentSessionStatus{AuditPublicKey: attackerPub},
		}
		changed, err := syncAuditKeyStatus(sess, seed)
		assert.Error(t, err, "seed %q is not a usable Ed25519 seed", seed)
		assert.False(t, changed, "a failed derivation reports no change")
	}
}

func auditKeyIDForTest(t *testing.T, seedHex string) string {
	t.Helper()
	_, keyID, err := auditPubFromSeed(seedHex)
	require.NoError(t, err)
	return keyID
}
