package provenance_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

// TestSignVerifyEndToEnd wires the REAL components — a memory.Local facade
// with verify-on-write enabled, a SigningMemory in front of it, and a
// WriteVerifier over a registered key — and proves the sign↔verify
// contract that Task 8's enforcement flip relies on:
//
//	(a) a signed append-only Put through the SigningMemory succeeds and
//	    round-trips (Get returns the entry WITH its provenance envelope);
//	(b) a DIRECT unsigned append-only Put to the Local is rejected with
//	    ErrProvenanceRequired (the operator's verify-on-write would catch
//	    any append-only writer that forgot to sign);
//	(c) an append-only Put signed by an UNREGISTERED key is rejected with
//	    ErrBadProvenance (a stray/forged key cannot pass).
//
// This is the integration counterpart to the facade unit tests (which use
// a fake verifier) and the writeverify unit tests (which exercise
// VerifyEntry in isolation): here the actual SigningMemory → Local →
// WriteVerifier path runs against a real backend.
func TestSignVerifyEndToEnd(t *testing.T) {
	registerKinds(t) // installs the append-only "test_audit" Kind; not parallel-safe
	ctx := memory.WithSystemApproval(context.Background(), "test")

	const publisher = "system:operator"
	priv := testKey(7)
	signer := provenance.NewSigner(priv, publisher)

	// The trusted key set holds ONLY publisher P's key under its keyID.
	keys := fakeKeys{{publisher, signer.KeyID()}: pub(priv)}

	local := memory.NewLocal(inmem.NewBackend(),
		memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	signed := provenance.NewSigningMemory(local, signer)

	t.Run("signed append-only Put succeeds and round-trips with provenance", func(t *testing.T) {
		in := auditEntry("ta-ok", `{"outcome":"allowed"}`)
		stored, err := signed.Put(ctx, in)
		require.NoError(t, err, "signed append-only Put must be accepted")
		require.NotNil(t, stored.Provenance, "stored entry carries provenance")
		assert.Equal(t, publisher, stored.Provenance.Publisher)
		assert.Equal(t, uint64(1), stored.Provenance.Seq, "first write opens the chain at seq 1")

		got, found, err := local.Get(ctx, in.Scope, in.Kind, in.ID)
		require.NoError(t, err)
		require.True(t, found, "entry persisted to the backend")
		require.NotNil(t, got.Provenance, "round-tripped entry retains its provenance")
		assert.Equal(t, signer.KeyID(), got.Provenance.KeyID)
		assert.NotEmpty(t, got.Provenance.Sig, "signature is persisted")
	})

	t.Run("direct unsigned append-only Put is rejected: ErrProvenanceRequired", func(t *testing.T) {
		// Bypass the SigningMemory: write straight to the verifying Local
		// with no provenance, exactly as a writer that forgot to sign would.
		_, err := local.Put(ctx, auditEntry("ta-unsigned", `{"outcome":"allowed"}`))
		require.Error(t, err)
		assert.ErrorIs(t, err, memory.ErrProvenanceRequired)
	})

	t.Run("append-only Put signed by an unregistered key is rejected: ErrBadProvenance", func(t *testing.T) {
		// A second signer for the SAME publisher but a DIFFERENT key that is
		// not in the trusted set. In-process writes have an empty caller, so
		// the publisher==caller check passes and the failure is the unknown
		// key, not a caller mismatch.
		strayPriv := testKey(99)
		straySigner := provenance.NewSigner(strayPriv, publisher)
		strayMem := provenance.NewSigningMemory(local, straySigner)

		_, err := strayMem.Put(ctx, auditEntry("ta-stray", `{"outcome":"allowed"}`))
		require.Error(t, err)
		assert.ErrorIs(t, err, memory.ErrBadProvenance)
	})
}
