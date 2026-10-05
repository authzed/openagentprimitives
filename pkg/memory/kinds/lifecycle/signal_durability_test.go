package lifecycle_test

import (
	"context"
	"crypto/ed25519"
	"path/filepath"
	"testing"
	"time"

	lc "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Verify stored bytes, rather than the signed Put response: SQLite's timestamp
// encoding used to turn a missing signal time into 1754, corrupting signatures
// and the next entry's chain link despite successful verify-on-write.
func TestSignalAuditSurvivesSQLiteReopen(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	scope := memory.Scope{Kind: "session", ID: "ns/signals"}
	path := filepath.Join(t.TempDir(), "memory.db")
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signer := provenance.NewSigner(priv, "system:operator")
	keys := provenance.MapKeyLookup{{Publisher: signer.Publisher(), KeyID: signer.KeyID()}: priv.Public().(ed25519.PublicKey)}
	open := func() (*memsqlite.Client, *memory.Local, *provenance.SigningMemory) {
		client, err := memsqlite.NewClient(path)
		require.NoError(t, err)
		require.NoError(t, client.Migrate(ctx))
		local := memory.NewLocal(memsqlite.NewBackend(client), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
		writer := provenance.NewSigningMemory(local, provenance.NewSigner(priv, "system:operator"))
		lifecycle.Setup(writer)
		return client, local, writer
	}
	defer lifecycle.Teardown()
	client, local, writer := open()
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	before := time.Now().UTC()
	provided := before.Add(-time.Hour).Truncate(time.Microsecond)
	require.NoError(t, local.SendSignal(ctx, memory.Signal{Scope: scope, Kind: lifecycle.SigSessionStarted}))
	require.NoError(t, local.SendSignal(ctx, memory.Signal{Scope: scope, Kind: lifecycle.SigTurnCompleted, At: provided}))
	// A caller-supplied nonzero time can overflow too. Refusing it must not
	// strand the signer on a position which never landed in storage.
	require.ErrorContains(t, local.SendSignal(ctx, memory.Signal{Scope: scope, Kind: lifecycle.SigTurnCompleted, At: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)}), "timestamp range")
	require.NoError(t, lifecycle.Append(ctx, writer, scope, lc.Stopped{}, time.Now().UTC(), lifecycle.OrderKey{}))
	verify := func(want int) {
		result, err := local.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"lifecycle"}})
		require.NoError(t, err)
		require.Len(t, result.Entries, want)
		report := provenance.NewVerifier(keys).VerifyChain("system:operator", result.Entries, nil)
		assert.Empty(t, report.Findings)
		assert.Equal(t, want, report.OK)
		for _, entry := range result.Entries {
			if len(entry.Tags) == 1 && entry.Tags[0] == lifecycle.SignalTag(lifecycle.SigTurnCompleted) {
				assert.True(t, provided.Equal(entry.CreatedAt), "explicit sender time is preserved")
			} else {
				assert.False(t, entry.CreatedAt.Before(before), "missing time uses receipt time before signing")
			}
		}
	}
	verify(3)
	require.NoError(t, client.Close())
	lifecycle.Teardown()
	client, local, _ = open()
	require.NoError(t, local.SendSignal(ctx, memory.Signal{Scope: scope, Kind: lifecycle.SigSessionIdle}))
	verify(4)
}
