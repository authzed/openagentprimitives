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

// TestComputeChainHeads tracks, per publisher, the highest-seq signed
// entry across two interleaved chains in the same scope. The head's
// LastHash must be EntryDigest of that publisher's max-seq entry.
func TestComputeChainHeads(t *testing.T) {
	registerKinds(t)

	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Two publishers, each signing into the same scope.
	signerA := provenance.NewSigningMemory(mem, provenance.NewSigner(testKey(31), "session:ns/sess"))
	signerB := provenance.NewSigningMemory(mem, provenance.NewSigner(testKey(32), "system:operator"))

	// Publisher A writes three entries (seq 1..3).
	var lastA memory.Entry
	for i := 0; i < 3; i++ {
		out, err := signerA.Put(ctx, auditEntry("ta-a"+string(rune('1'+i)), `{"i":`+string(rune('0'+i))+`}`))
		require.NoError(t, err, "publisher A put %d", i)
		lastA = out
	}

	// Publisher B writes two entries (seq 1..2).
	var lastB memory.Entry
	for i := 0; i < 2; i++ {
		out, err := signerB.Put(ctx, auditEntry("ta-b"+string(rune('1'+i)), `{"j":`+string(rune('0'+i))+`}`))
		require.NoError(t, err, "publisher B put %d", i)
		lastB = out
	}

	heads, err := provenance.ComputeChainHeads(ctx, mem, scope)
	require.NoError(t, err, "ComputeChainHeads must succeed")
	require.Len(t, heads, 2, "one head per publisher")

	hA, ok := heads["session:ns/sess"]
	require.True(t, ok, "publisher A head present")
	assert.Equal(t, uint64(3), hA.Seq, "publisher A head at max seq 3")
	assert.Equal(t, provenance.EntryDigest(lastA), hA.LastHash, "publisher A head hash is its max-seq entry digest")

	hB, ok := heads["system:operator"]
	require.True(t, ok, "publisher B head present")
	assert.Equal(t, uint64(2), hB.Seq, "publisher B head at max seq 2")
	assert.Equal(t, provenance.EntryDigest(lastB), hB.LastHash, "publisher B head hash is its max-seq entry digest")
}

// TestComputeChainHeadsSkipsUnsigned ensures nil-provenance (legacy)
// entries anchor nothing — only signed publishers get a head, and the
// scan over an empty scope returns an empty map without error.
func TestComputeChainHeadsSkipsUnsigned(t *testing.T) {
	registerKinds(t)

	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Empty scope → no heads.
	heads, err := provenance.ComputeChainHeads(ctx, mem, scope)
	require.NoError(t, err)
	assert.Empty(t, heads, "empty scope yields no chain heads")

	// A single signed entry yields exactly one head.
	signing := provenance.NewSigningMemory(mem, provenance.NewSigner(testKey(33), "session:ns/sess"))
	out, err := signing.Put(ctx, auditEntry("ta-1", `{"i":0}`))
	require.NoError(t, err)

	heads, err = provenance.ComputeChainHeads(ctx, mem, scope)
	require.NoError(t, err)
	require.Len(t, heads, 1, "one signed publisher → one head")
	assert.Equal(t, provenance.EntryDigest(out), heads["session:ns/sess"].LastHash)
}
