package main

import (
	"context"
	"crypto/ed25519"
	"path/filepath"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/shadow"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

func TestOperatorAuditSeedUsesDurableShadowAfterRestart(t *testing.T) {
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
	db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "audit.db"))
	require.NoError(t, err)
	require.NoError(t, db.Migrate(ctx))
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	durable := memsqlite.NewBackend(db)
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	keys := provenance.MapKeyLookup{{Publisher: "system:operator", KeyID: provenance.KeyID(priv.Public().(ed25519.PublicKey))}: priv.Public().(ed25519.PublicKey)}
	writer := func() (*provenance.SigningMemory, memory.Memory) {
		backend, err := shadow.New(meminmem.NewBackend(), durable, shadow.ReadFromPrimary, logr.Discard())
		require.NoError(t, err)
		local := memory.NewLocal(backend, memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
		signed := provenance.NewSigningMemory(local, provenance.NewSigner(priv, "system:operator"), provenance.WithSeedMemory(auditSeedMemory{Memory: local}))
		return signed, local
	}
	scope := memory.Scope{Kind: "session", ID: "team/source"}
	entry := func(id string) memory.Entry {
		return memory.Entry{Scope: scope, Kind: goalconsent.KindName, ID: id, CreatedAt: time.Now().UTC().Truncate(time.Microsecond), Content: []byte(`{"goal":{"title":"Reminder"}}`)}
	}
	first, _ := writer()
	_, err = first.Put(ctx, entry("goalconsent-before-restart"))
	require.NoError(t, err)
	second, local := writer()
	stale, err := local.Query(ctx, memory.Query{Scope: scope})
	require.NoError(t, err)
	require.Empty(t, stale.Entries, "ephemeral primary starts empty after operator restart")
	added, err := second.Put(ctx, entry("goalconsent-after-restart"))
	require.NoError(t, err)
	require.Equal(t, uint64(2), added.Provenance.Seq)
	records, err := local.Query(shadow.WithReadFrom(ctx, shadow.ReadFromSecondary), memory.Query{Scope: scope})
	require.NoError(t, err)
	require.Len(t, records.Entries, 2)
	report := provenance.NewVerifier(keys).VerifyChain("system:operator", records.Entries, nil)
	require.Empty(t, report.Findings)
}
