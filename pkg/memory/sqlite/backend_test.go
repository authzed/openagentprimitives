package sqlite_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
)

// newTestClient opens a fresh migrated DB in a temp dir. Returns the
// client and the file path (so tests can reopen to assert durability).
func newTestClient(t *testing.T) (*memsqlite.Client, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.db")
	c, err := memsqlite.NewClient(path)
	require.NoError(t, err, "NewClient")
	require.NoError(t, c.Migrate(context.Background()), "Migrate")
	t.Cleanup(func() { _ = c.Close() })
	return c, path
}

func TestClient_MigrateCreatesTables(t *testing.T) {
	c, _ := newTestClient(t)
	row := c.DB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('memory_entry','memory_link')`)
	var n int
	require.NoError(t, row.Scan(&n))
	assert.Equal(t, 2, n, "both tables must exist after Migrate")
}

func TestClient_MigrateIsIdempotent(t *testing.T) {
	c, _ := newTestClient(t)
	assert.NoError(t, c.Migrate(context.Background()), "second Migrate must be a no-op")
}

func TestBackend_Capabilities_ParityWithPostgres(t *testing.T) {
	c, _ := newTestClient(t)
	caps := memsqlite.NewBackend(c).Capabilities()
	assert.True(t, caps.ContentSchemas)
	assert.True(t, caps.FieldRange)
	assert.Equal(t, -1, caps.LinkTraversal)
	assert.True(t, caps.ReverseLinks)
	assert.True(t, caps.TagFilters)
	assert.True(t, caps.TimeRange)
}

func TestBackend_PutGetRoundtrip(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	created := time.Now().UTC().Truncate(time.Nanosecond)
	e := memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1",
		CreatedAt: created,
		Tags:      []string{"x", "y"},
		Content:   json.RawMessage(`{"status":"active","n":3}`),
		Links:     []memory.Link{{Relation: "of", Scope: scope, Kind: "beta", ID: "b-1"}},
	}
	require.NoError(t, b.Put(ctx, e))

	got, ok, err := b.Get(ctx, scope, "alpha", "a-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, e.Kind, got.Kind)
	assert.Equal(t, e.ID, got.ID)
	assert.True(t, created.Equal(got.CreatedAt), "createdAt round-trips exactly")
	assert.Equal(t, []string{"x", "y"}, got.Tags)
	assert.JSONEq(t, `{"status":"active","n":3}`, string(got.Content))
	require.Len(t, got.Links, 1)
	assert.Equal(t, "beta", got.Links[0].Kind)
}

func TestBackend_GetMissingReturnsFalse(t *testing.T) {
	c, _ := newTestClient(t)
	_, ok, err := memsqlite.NewBackend(c).Get(context.Background(),
		memory.Scope{Kind: "session", ID: "ns/a"}, "alpha", "nope")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestBackend_PutLastWriterWins(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	base := memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC()}
	require.NoError(t, b.Put(ctx, base))
	base.Content = json.RawMessage(`{"v":2}`)
	require.NoError(t, b.Put(ctx, base))
	got, _, err := b.Get(ctx, scope, "alpha", "a-1")
	require.NoError(t, err)
	assert.JSONEq(t, `{"v":2}`, string(got.Content))
}

func TestBackend_GetCorruptLinksSurfacesError(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC(),
	}))

	_, err := c.DB().ExecContext(ctx,
		`UPDATE memory_entry SET links = ? WHERE scope_kind=? AND scope_id=? AND kind=? AND entry_id=?`,
		"{not valid json", scope.Kind, scope.ID, "alpha", "a-1")
	require.NoError(t, err, "corrupt the stored links blob directly")

	_, _, err = b.Get(ctx, scope, "alpha", "a-1")
	require.Error(t, err, "a corrupt links blob must surface, not read back as no-links")
	assert.Contains(t, err.Error(), "unmarshal links")
}

func TestBackend_GetCorruptTagsSurfacesError(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC(),
	}))

	_, err := c.DB().ExecContext(ctx,
		`UPDATE memory_entry SET tags = ? WHERE scope_kind=? AND scope_id=? AND kind=? AND entry_id=?`,
		"{not valid json", scope.Kind, scope.ID, "alpha", "a-1")
	require.NoError(t, err, "corrupt the stored tags blob directly")

	_, _, err = b.Get(ctx, scope, "alpha", "a-1")
	require.Error(t, err, "a corrupt tags blob must surface, not read back as no-tags")
	assert.Contains(t, err.Error(), "unmarshal tags")
}

// appendOnlyEntry builds an entry of a kind that REALLY declares itself
// append-only, so the provenance tests below are anchored to the shipped
// Retention contract rather than to a made-up kind name. The precondition is
// asserted, not assumed: if `turn` ever stops being append-only, these tests
// say so instead of silently testing nothing.
func appendOnlyEntry(t *testing.T, scope memory.Scope, id string) memory.Entry {
	t.Helper()
	require.True(t, turn.Kind{}.Retention().AppendOnly,
		"precondition: %q must be an append-only kind for a provenance test to mean anything", turn.KindName)
	return memory.Entry{
		Scope:     scope,
		Kind:      turn.KindName,
		ID:        turn.IDPrefix + id,
		CreatedAt: time.Now().UTC(),
		Content:   json.RawMessage(`{"role":"user","text":"hello"}`),
	}
}

// TestBackend_AppendOnlyEntryRoundTripsProvenance pins the storage half of the
// tamper-evident audit log (AGENTS.md §Tamper-evident audit log): the envelope
// an append-only entry is signed with must come back out of the backend intact.
// Dropping it is silent — a nil envelope reads back as merely "unsigned", which
// `oap audit verify` reports as a warning and exits 0 on.
func TestBackend_AppendOnlyEntryRoundTripsProvenance(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	e := appendOnlyEntry(t, scope, "0-user")
	e.Provenance = &memory.Provenance{
		Publisher: "system:test",
		KeyID:     "abcd1234",
		Seq:       7,
		PrevHash:  "deadbeef",
		Sig:       []byte{1, 2, 3, 4},
	}
	require.NoError(t, b.Put(ctx, e))

	got, ok, err := b.Get(ctx, scope, e.Kind, e.ID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, e.Provenance, got.Provenance, "Get must round-trip the full provenance envelope")

	// Query uses the same scan path but a different column list — cover both.
	res, err := b.Query(ctx, memory.Query{Scope: scope, Kinds: []string{turn.KindName}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, e.Provenance, res.Entries[0].Provenance, "Query must round-trip the full provenance envelope")
}

// TestBackend_SignedAppendOnlyChainSurvivesRoundTrip is the end-to-end
// guarantee: a chain signed by a real Ed25519 publisher must still verify —
// signature, seq contiguity, prevHash linkage, and the tail anchor — after
// going through this backend. It is what `oap audit verify` does offline.
func TestBackend_SignedAppendOnlyChainSurvivesRoundTrip(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err, "generate publisher key")
	publisher := provenance.SessionPublisher("default", "demo-session")
	signer := provenance.NewSigner(priv, publisher)

	var written []memory.Entry
	for _, id := range []string{"0-user", "1-assistant"} {
		e := appendOnlyEntry(t, scope, id)
		require.NoError(t, signer.Sign(&e), "sign %s", e.ID)
		require.NoError(t, b.Put(ctx, e))
		written = append(written, e)
	}

	res, err := b.Query(ctx, memory.Query{
		Scope: scope, Kinds: []string{turn.KindName},
		OrderBy: memory.OrderBy{Field: "createdAt"},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 2)

	// The digest is both the chain link and the signed message: if the stored
	// bytes reshape it, every downstream check fails for the wrong reason.
	for i, w := range written {
		assert.Equal(t, provenance.EntryDigest(w), provenance.EntryDigest(res.Entries[i]),
			"digest of %s must survive the round-trip", w.ID)
	}

	// The tail anchor is the truncation guard, recorded externally on
	// AgentSession.status — verify against it, not just against the entries.
	last := written[len(written)-1]
	anchor := &provenance.ChainHead{Seq: last.Provenance.Seq, LastHash: provenance.EntryDigest(last)}
	keys := provenance.MapKeyLookup{
		{Publisher: publisher, KeyID: signer.KeyID()}: pub,
	}
	rep := provenance.NewVerifier(keys).VerifyChain(publisher, res.Entries, anchor)
	assert.Empty(t, rep.Findings, "a signed chain must verify clean after a sqlite round-trip")
	assert.Equal(t, 2, rep.OK, "both entries must count as verified")
}

// TestBackend_GetCorruptProvenanceSurfacesError: a corrupt envelope must be
// surfaced, never laundered into a nil one. nil reads back as "unsigned", which
// silently lowers the re-seeded chain head and the truncation anchor — the
// exact failure that makes a fabricated audit row indistinguishable.
func TestBackend_GetCorruptProvenanceSurfacesError(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	e := appendOnlyEntry(t, scope, "0-user")
	e.Provenance = &memory.Provenance{Publisher: "system:test", KeyID: "abcd1234", Seq: 1}
	require.NoError(t, b.Put(ctx, e))

	_, err := c.DB().ExecContext(ctx,
		`UPDATE memory_entry SET provenance = ? WHERE scope_kind=? AND scope_id=? AND kind=? AND entry_id=?`,
		"{not valid json", scope.Kind, scope.ID, e.Kind, e.ID)
	require.NoError(t, err, "corrupt the stored provenance blob directly")

	_, _, err = b.Get(ctx, scope, e.Kind, e.ID)
	require.Error(t, err, "a corrupt provenance blob must surface, not read back as unsigned")
	assert.Contains(t, err.Error(), "unmarshal provenance")
}

// TestClient_MigrateAddsProvenanceColumnToPopulatedPreExistingDB covers the
// upgrade path for databases already on disk from the shipped sqlite install
// shape (`oap init --local`, `oap desktop`), which were created before the column
// existed. CREATE TABLE IF NOT EXISTS is a no-op against them, so without an
// additive migration those installs would keep dropping the envelope forever.
//
// Migrate runs on every operator startup against those live databases, so the
// upgrade is asserted under the conditions that actually break users: the table
// already holds rows, and Migrate is called more than once.
func TestClient_MigrateAddsProvenanceColumnToPopulatedPreExistingDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	c, err := memsqlite.NewClient(path)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })

	// The historical memory_entry shape, verbatim minus the provenance column.
	_, err = c.DB().ExecContext(ctx, `
		CREATE TABLE memory_entry (
		    scope_kind TEXT NOT NULL,
		    scope_id   TEXT NOT NULL,
		    kind       TEXT NOT NULL,
		    entry_id   TEXT NOT NULL,
		    created_at INTEGER NOT NULL,
		    tags       TEXT NOT NULL DEFAULT '[]',
		    content    TEXT,
		    links      TEXT NOT NULL DEFAULT '[]',
		    PRIMARY KEY (scope_kind, scope_id, kind, entry_id)
		)`)
	require.NoError(t, err, "seed the pre-provenance schema")

	// A row written by the old code, i.e. the state of a real desktop install.
	legacyID := turn.IDPrefix + "0-user"
	_, err = c.DB().ExecContext(ctx,
		`INSERT INTO memory_entry (scope_kind, scope_id, kind, entry_id, created_at, tags, content, links)
		 VALUES (?, ?, ?, ?, ?, '[]', ?, '[]')`,
		"session", "ns/a", turn.KindName, legacyID, time.Now().UTC().UnixNano(),
		`{"role":"user","text":"written before the column existed"}`)
	require.NoError(t, err, "seed a pre-existing row")

	// Twice: the operator calls Migrate on every startup, so a migration that
	// is not a no-op the second time crashloops the install it just upgraded.
	require.NoError(t, c.Migrate(ctx), "Migrate must upgrade an existing populated database in place")
	require.NoError(t, c.Migrate(ctx), "a second Migrate over the upgraded database must be a no-op")

	var n int
	require.NoError(t, c.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('memory_entry') WHERE name = 'provenance'`).Scan(&n))
	assert.Equal(t, 1, n, "Migrate must add the provenance column exactly once")

	b := memsqlite.NewBackend(c)
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	// The pre-existing row must survive, readable, with a nil (never a garbled)
	// envelope — those rows genuinely predate the chain and read as unsigned.
	legacy, ok, err := b.Get(ctx, scope, turn.KindName, legacyID)
	require.NoError(t, err, "a row written before the migration must still read back")
	require.True(t, ok, "the migration must not drop pre-existing rows")
	assert.Nil(t, legacy.Provenance, "a pre-migration row has no envelope to report")
	assert.JSONEq(t, `{"role":"user","text":"written before the column existed"}`, string(legacy.Content))

	// And the upgraded database must actually carry an envelope end to end.
	e := appendOnlyEntry(t, scope, "1-assistant")
	e.Provenance = &memory.Provenance{Publisher: "system:test", KeyID: "abcd1234", Seq: 1, Sig: []byte{9}}
	require.NoError(t, b.Put(ctx, e))
	got, ok, err := b.Get(ctx, scope, e.Kind, e.ID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, e.Provenance, got.Provenance)
}

func TestBackend_DurabilityAcrossReopen(t *testing.T) {
	c, path := newTestClient(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, memsqlite.NewBackend(c).Put(ctx, memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, c.Close())

	// Reopen the SAME file in a fresh client — the entry must persist.
	c2, err := memsqlite.NewClient(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c2.Close() })
	_, ok, err := memsqlite.NewBackend(c2).Get(ctx, scope, "alpha", "a-1")
	require.NoError(t, err)
	assert.True(t, ok, "entry must survive close + reopen")
}

func TestBackend_DeleteIsIdempotent(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(ctx, memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC()}))
	require.NoError(t, b.Delete(ctx, scope, "alpha", "a-1"))
	_, ok, err := b.Get(ctx, scope, "alpha", "a-1")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.NoError(t, b.Delete(ctx, scope, "alpha", "a-1"), "deleting again is not an error")
}

func TestBackend_DeleteScopeRemovesAll(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	for _, id := range []string{"a-1", "a-2"} {
		require.NoError(t, b.Put(ctx, memory.Entry{Scope: scope, Kind: "alpha", ID: id, CreatedAt: time.Now().UTC()}))
	}
	require.NoError(t, b.DeleteScope(ctx, scope))
	st, err := b.Status(ctx, scope)
	require.NoError(t, err)
	assert.Equal(t, memory.StatusUnknown, st.State)
}

func TestBackend_StatusLiveWhenPopulated(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	st, err := b.Status(ctx, scope)
	require.NoError(t, err)
	assert.Equal(t, memory.StatusUnknown, st.State)

	require.NoError(t, b.Put(ctx, memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC()}))
	st, err = b.Status(ctx, scope)
	require.NoError(t, err)
	assert.Equal(t, memory.StatusLive, st.State)
}

func seedQueryEntries(t *testing.T, b *memsqlite.Backend, scope memory.Scope) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	put := func(kind, id string, ageMin int, tags []string, content string, links []memory.Link) {
		require.NoError(t, b.Put(ctx, memory.Entry{
			Scope: scope, Kind: kind, ID: id,
			CreatedAt: base.Add(time.Duration(ageMin) * time.Minute),
			Tags:      tags, Content: json.RawMessage(content), Links: links,
		}))
	}
	put("alpha", "a-1", 0, []string{"red"}, `{"status":"active","n":1}`, nil)
	put("alpha", "a-2", 10, []string{"red", "blue"}, `{"status":"done","n":5}`, []memory.Link{{Relation: "of", Scope: scope, Kind: "beta", ID: "b-1"}})
	put("beta", "b-1", 20, []string{"blue"}, `{"status":"active","n":9}`, nil)
}

func TestBackend_QueryByKind(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	seedQueryEntries(t, b, scope)
	res, err := b.Query(context.Background(), memory.Query{Scope: scope, Kinds: []string{"alpha"}})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 2)
}

func TestBackend_QueryByTagsRequiresAll(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	seedQueryEntries(t, b, scope)
	res, err := b.Query(context.Background(), memory.Query{Scope: scope, Tags: []string{"red", "blue"}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "a-2", res.Entries[0].ID)
}

func TestBackend_QueryTimeRangeAndOrder(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	seedQueryEntries(t, b, scope)
	since := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	res, err := b.Query(context.Background(), memory.Query{
		Scope: scope, Since: &since, OrderBy: memory.OrderBy{Field: "createdAt", Desc: true},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 2)
	assert.Equal(t, "b-1", res.Entries[0].ID, "newest first")
	assert.Equal(t, "a-2", res.Entries[1].ID)
}

func TestBackend_QueryFieldEquals(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	seedQueryEntries(t, b, scope)
	res, err := b.Query(context.Background(), memory.Query{
		Scope:       scope,
		FieldEquals: []memory.FieldFilter{{Path: "status", Op: memory.FieldOpEq, Value: "active"}},
	})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 2) // a-1 and b-1
}

func TestBackend_QueryFieldRange(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	seedQueryEntries(t, b, scope)
	res, err := b.Query(context.Background(), memory.Query{
		Scope:       scope,
		FieldEquals: []memory.FieldFilter{{Path: "n", Op: memory.FieldOpGte, Value: 5}},
	})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 2) // n=5 and n=9
}

func TestBackend_QueryLinkedTo(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	seedQueryEntries(t, b, scope)
	res, err := b.Query(context.Background(), memory.Query{
		Scope:    scope,
		LinkedTo: []memory.LinkFilter{{Kind: "beta", ID: "b-1"}},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "a-2", res.Entries[0].ID)
}

func TestBackend_QueryLinkedFrom(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	seedQueryEntries(t, b, scope)
	res, err := b.Query(context.Background(), memory.Query{
		Scope:      scope,
		LinkedFrom: []memory.LinkFilter{{Kind: "alpha", ID: "a-2"}},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "b-1", res.Entries[0].ID)
}

func TestBackend_QueryLimit(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	seedQueryEntries(t, b, scope)
	res, err := b.Query(context.Background(), memory.Query{Scope: scope, Limit: 1})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 1)
}

// TestBackend_QueryAllScopes covers Part 2 (not in the original task brief,
// added because main grew memory.Backend.QueryAllScopes after this task
// was planned): seeds entries across TWO scope_ids under the same
// ScopeKind and confirms the cross-scope query spans both, honoring
// OrderDesc + Limit/Offset. Mirrors postgres's
// TestQueryAllScopes_Postgres and inmem's TestQueryAllScopes_Inmem.
func TestBackend_QueryAllScopes(t *testing.T) {
	c, _ := newTestClient(t)
	b := memsqlite.NewBackend(c)
	ctx := context.Background()
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	mk := func(scopeID, kind, id string, at time.Time) memory.Entry {
		return memory.Entry{Scope: memory.Scope{Kind: "session", ID: scopeID},
			Kind: kind, ID: id, CreatedAt: at, Content: json.RawMessage(`{"x":1}`)}
	}
	require.NoError(t, b.Put(ctx, mk("default/s1", "approval", "approval-1", t0)))
	require.NoError(t, b.Put(ctx, mk("default/s2", "approval", "approval-2", t0.Add(time.Minute))))
	require.NoError(t, b.Put(ctx, mk("default/s2", "turn", "turn-0-user", t0.Add(2*time.Minute))))
	// Different scope KIND — must never match a "session" cross-scope query.
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: memory.Scope{Kind: "global", ID: "g"}, Kind: "approval", ID: "approval-g",
		CreatedAt: t0, Content: json.RawMessage(`{}`),
	}))

	t.Run("kinds filter spans all scopes, newest first", func(t *testing.T) {
		res, err := b.QueryAllScopes(ctx, memory.CrossScopeQuery{
			ScopeKind: "session", Kinds: []string{"approval"}, OrderDesc: true,
		})
		require.NoError(t, err)
		require.Len(t, res.Entries, 2)
		assert.Equal(t, "approval-2", res.Entries[0].ID)
		assert.Equal(t, "default/s2", res.Entries[0].Scope.ID, "scope must ride along on each entry")
		assert.Equal(t, "approval-1", res.Entries[1].ID)
		assert.Equal(t, "default/s1", res.Entries[1].Scope.ID)
	})

	t.Run("time range + limit + offset paginate deterministically", func(t *testing.T) {
		since := t0.Add(30 * time.Second)
		res, err := b.QueryAllScopes(ctx, memory.CrossScopeQuery{
			ScopeKind: "session", Kinds: []string{"approval", "turn"},
			Since: &since, OrderDesc: true, Limit: 1, Offset: 1,
		})
		require.NoError(t, err)
		require.Len(t, res.Entries, 1)
		assert.Equal(t, "approval-2", res.Entries[0].ID)
	})
}
