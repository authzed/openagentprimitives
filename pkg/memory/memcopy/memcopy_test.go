package memcopy_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/memcopy"
)

func newMem(t *testing.T) memory.Memory {
	t.Helper()
	return memory.NewLocal(inmem.NewBackend())
}

func TestCopyPrefix_TurnsOnly(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)
	src := memory.Scope{Kind: "AgentSession", ID: "ns/src"}
	dst := memory.Scope{Kind: "AgentSession", ID: "ns/dst"}

	appender := turn.NewAppender(mem, src)
	for i := 0; i < 5; i++ {
		require.NoError(t, appender.Append(ctx, memory.Turn{
			Index: i, Role: "user", CreatedAt: time.Unix(int64(i), 0),
			Content: []memory.ContentBlock{{Type: "text", Text: "u"}},
		}))
	}

	require.NoError(t, memcopy.CopyPrefix(ctx, mem, src, dst, 2))

	got, err := turn.NewAppender(mem, dst).ReadAll(ctx)
	require.NoError(t, err)
	require.Len(t, got, 3, "turns 0,1,2 inclusive")
	assert.Equal(t, 0, got[0].Index)
	assert.Equal(t, 1, got[1].Index)
	assert.Equal(t, 2, got[2].Index)
}

func TestCopyPrefix_KeepsNonAnchoredEntries(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)
	src := memory.Scope{Kind: "AgentSession", ID: "ns/src"}
	dst := memory.Scope{Kind: "AgentSession", ID: "ns/dst"}

	raw, _ := json.Marshal(struct{ Value string }{"x"})
	_, err := mem.Put(ctx, memory.Entry{
		Scope: src, Kind: "lineage", ID: "lineage-fork-in-something",
		CreatedAt: time.Now().UTC(), Content: raw,
	})
	require.NoError(t, err)

	require.NoError(t, memcopy.CopyPrefix(ctx, mem, src, dst, 0))

	res, err := mem.Query(ctx, memory.Query{Scope: dst, Kinds: []string{"lineage"}})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 1)
}

func TestCopyPrefix_DropsEntriesAnchoredAboveCut(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)
	src := memory.Scope{Kind: "AgentSession", ID: "ns/src"}
	dst := memory.Scope{Kind: "AgentSession", ID: "ns/dst"}

	require.NoError(t, turn.NewAppender(mem, src).Append(ctx, memory.Turn{
		Index: 3, Role: "user", CreatedAt: time.Now().UTC(),
		Content: []memory.ContentBlock{{Type: "text", Text: "u"}},
	}))

	raw, _ := json.Marshal(struct{ Value string }{"x"})
	_, err := mem.Put(ctx, memory.Entry{
		Scope: src, Kind: "tool_dispatch_snapshot",
		ID:        "tds-000003-000-tu_x",
		CreatedAt: time.Now().UTC(),
		Content:   raw,
		Links: []memory.Link{{
			Relation: "for_turn",
			Kind:     turn.KindName,
			ID:       turn.EntryID(3, "assistant"),
		}},
	})
	require.NoError(t, err)

	require.NoError(t, memcopy.CopyPrefix(ctx, mem, src, dst, 2))

	turns, err := turn.NewAppender(mem, dst).ReadAll(ctx)
	require.NoError(t, err)
	assert.Empty(t, turns, "turn-3 dropped")

	res, err := mem.Query(ctx, memory.Query{Scope: dst, Kinds: []string{"tool_dispatch_snapshot"}})
	require.NoError(t, err)
	assert.Empty(t, res.Entries)
}

func TestCopyPrefix_KeepsEntriesAnchoredAtOrBelowCut(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)
	src := memory.Scope{Kind: "AgentSession", ID: "ns/src"}
	dst := memory.Scope{Kind: "AgentSession", ID: "ns/dst"}

	require.NoError(t, turn.NewAppender(mem, src).Append(ctx, memory.Turn{
		Index: 2, Role: "user", CreatedAt: time.Now().UTC(),
		Content: []memory.ContentBlock{{Type: "text", Text: "u"}},
	}))

	raw, _ := json.Marshal(struct{ Value string }{"x"})
	_, err := mem.Put(ctx, memory.Entry{
		Scope: src, Kind: "tool_dispatch_snapshot",
		ID:        "tds-000002-000-tu_x",
		CreatedAt: time.Now().UTC(),
		Content:   raw,
		Links: []memory.Link{{
			Relation: "for_turn",
			Kind:     turn.KindName,
			ID:       turn.EntryID(2, "assistant"),
		}},
	})
	require.NoError(t, err)

	require.NoError(t, memcopy.CopyPrefix(ctx, mem, src, dst, 2))

	res, err := mem.Query(ctx, memory.Query{Scope: dst, Kinds: []string{"tool_dispatch_snapshot"}})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 1, "anchored at cut → kept")
}

func TestCopyPrefix_ProvenancePreservedAndIsolated(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)
	src := memory.Scope{Kind: "AgentSession", ID: "ns/src"}
	dst := memory.Scope{Kind: "AgentSession", ID: "ns/dst"}

	raw, _ := json.Marshal(struct{ Value string }{"x"})
	origSig := []byte{10, 20, 30}
	_, err := mem.Put(ctx, memory.Entry{
		Scope: src, Kind: "lineage", ID: "lineage-fork-in-prov",
		CreatedAt: time.Now().UTC(), Content: raw,
		Provenance: &memory.Provenance{
			Publisher: "system:test",
			KeyID:     "cafe1234",
			Seq:       1,
			PrevHash:  "",
			Sig:       origSig,
		},
	})
	require.NoError(t, err)

	require.NoError(t, memcopy.CopyPrefix(ctx, mem, src, dst, 0))

	res, err := mem.Query(ctx, memory.Query{Scope: dst, Kinds: []string{"lineage"}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	prov := res.Entries[0].Provenance
	require.NotNil(t, prov)
	assert.Equal(t, "system:test", prov.Publisher)
	assert.Equal(t, []byte{10, 20, 30}, prov.Sig)

	// Mutating the original's Sig must not affect the copy.
	srcRes, err := mem.Query(ctx, memory.Query{Scope: src, Kinds: []string{"lineage"}})
	require.NoError(t, err)
	require.Len(t, srcRes.Entries, 1)
	if srcRes.Entries[0].Provenance != nil {
		srcRes.Entries[0].Provenance.Sig[0] = 99
	}
	// Re-read the destination to confirm isolation (inmem stores a copy).
	res2, err := mem.Query(ctx, memory.Query{Scope: dst, Kinds: []string{"lineage"}})
	require.NoError(t, err)
	require.Len(t, res2.Entries, 1)
	assert.Equal(t, []byte{10, 20, 30}, res2.Entries[0].Provenance.Sig)
}

func TestCopyPrefix_Idempotent(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)
	src := memory.Scope{Kind: "AgentSession", ID: "ns/src"}
	dst := memory.Scope{Kind: "AgentSession", ID: "ns/dst"}

	require.NoError(t, turn.NewAppender(mem, src).Append(ctx, memory.Turn{
		Index: 0, Role: "user", CreatedAt: time.Now().UTC(),
		Content: []memory.ContentBlock{{Type: "text", Text: "u"}},
	}))

	for i := 0; i < 3; i++ {
		require.NoError(t, memcopy.CopyPrefix(ctx, mem, src, dst, 5))
	}
	got, err := turn.NewAppender(mem, dst).ReadAll(ctx)
	require.NoError(t, err)
	assert.Len(t, got, 1, "idempotent re-copy must not duplicate")
}
