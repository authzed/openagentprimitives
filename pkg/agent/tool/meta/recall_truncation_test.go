package meta_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// cappingKG is a KGProvider double that honors the limit it is given, the way
// Graphiti's MaxFacts does. A fake that ignored the limit would let a tool that
// never probes look like one that does.
type cappingKG struct {
	facts    []memory.KGFact
	entities []memory.KGEntity
	gotLimit int
}

func (*cappingKG) Ingest(_ context.Context, _ memory.KGInput) error { return nil }
func (f *cappingKG) SearchFacts(_ context.Context, _ string, limit int) ([]memory.KGFact, error) {
	f.gotLimit = limit
	if limit > 0 && len(f.facts) > limit {
		return f.facts[:limit], nil
	}
	return f.facts, nil
}
func (*cappingKG) GetEntity(_ context.Context, _ string) (*memory.KGEntity, error) { return nil, nil }
func (f *cappingKG) EntityFacts(_ context.Context, _ string) ([]memory.KGFact, error) {
	return f.facts, nil
}
func (f *cappingKG) RelatedEntities(_ context.Context, _ string, limit int) ([]memory.KGEntity, error) {
	f.gotLimit = limit
	if limit > 0 && len(f.entities) > limit {
		return f.entities[:limit], nil
	}
	return f.entities, nil
}
func (*cappingKG) Communities(_ context.Context, _ string) ([]memory.KGCommunity, error) {
	return nil, nil
}

func kgFacts(n int) []memory.KGFact {
	out := make([]memory.KGFact, n)
	for i := range out {
		out[i] = memory.KGFact{UUID: fmt.Sprintf("f%d", i), Name: "knows", Fact: "a knows b"}
	}
	return out
}

func kgEntities(n int) []memory.KGEntity {
	out := make([]memory.KGEntity, n)
	for i := range out {
		out[i] = memory.KGEntity{UUID: fmt.Sprintf("e%d", i), Name: "thing"}
	}
	return out
}

func memEntries(n int) []memory.Entry {
	out := make([]memory.Entry, n)
	for i := range out {
		out[i] = memory.Entry{Kind: "turn", ID: fmt.Sprintf("turn-%06d-user", i), Content: json.RawMessage(`{}`)}
	}
	return out
}

// TestRecallToolsReportTruncation pins that the agent is told when a recall
// stopped at its limit. The model is the reader least able to notice a partial
// answer for itself — it cannot re-run with a bigger number out of suspicion —
// and a `count` that is silently a page size is exactly the input that gets
// reasoned over as if it were a total.
func TestRecallToolsReportTruncation(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sessFor := func(m tool.MemoryQuerier) *tool.SessionContext {
		return &tool.SessionContext{Namespace: "default", Name: "sess1", Mem: m}
	}

	t.Run("query_memory relays the facade's truncation verdict", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			truncated bool
			want      string
		}{
			{name: "truncated read is declared", truncated: true, want: `"truncated":true`},
			{name: "complete read is declared complete, not left silent", truncated: false, want: `"truncated":false`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fm := &fakeMemSearcher{queryResult: memory.QueryResult{Entries: memEntries(3), Truncated: tc.truncated}}
				res, err := meta.NewQueryMemory().Execute(ctx, json.RawMessage(`{"kinds":["turn"],"limit":3}`), sessFor(fm))
				require.NoError(t, err)
				assert.Contains(t, res.Content, tc.want)
			})
		}
	})

	t.Run("search_memory relays the searcher's truncation verdict", func(t *testing.T) {
		fm := &fakeMemSearcher{result: memory.MergedSearchResult{
			Entries:   []memory.ScoredEntry{{Entry: memory.Entry{Kind: "turn", ID: "turn-000001-user"}, Score: 1, Source: "pg"}},
			Truncated: true,
		}}
		res, err := meta.NewSearchMemory().Execute(ctx, json.RawMessage(`{"text":"x","limit":1}`), sessFor(fm))
		require.NoError(t, err)
		assert.Contains(t, res.Content, `"truncated":true`)
	})

	t.Run("query_knowledge probes the graph itself: search mode", func(t *testing.T) {
		kg := &cappingKG{facts: kgFacts(5)}
		sess := &tool.SessionContext{Namespace: "default", Name: "sess1", KG: kg}
		res, err := meta.NewQueryKnowledge().Execute(ctx, json.RawMessage(`{"mode":"search","text":"x","limit":3}`), sess)
		require.NoError(t, err)
		assert.Equal(t, 4, kg.gotLimit, "the graph must be asked for one fact past the limit")
		assert.Contains(t, res.Content, `"truncated":true`)
		assert.Contains(t, res.Content, `"count":3`, "the probe fact must never be counted or returned")
	})

	t.Run("query_knowledge search mode at exactly the limit is not truncated", func(t *testing.T) {
		kg := &cappingKG{facts: kgFacts(3)}
		sess := &tool.SessionContext{Namespace: "default", Name: "sess1", KG: kg}
		res, err := meta.NewQueryKnowledge().Execute(ctx, json.RawMessage(`{"mode":"search","text":"x","limit":3}`), sess)
		require.NoError(t, err)
		assert.Contains(t, res.Content, `"truncated":false`)
		assert.Contains(t, res.Content, `"count":3`)
	})

	t.Run("query_knowledge probes the graph itself: related mode", func(t *testing.T) {
		kg := &cappingKG{entities: kgEntities(5)}
		sess := &tool.SessionContext{Namespace: "default", Name: "sess1", KG: kg}
		res, err := meta.NewQueryKnowledge().Execute(ctx, json.RawMessage(`{"mode":"related","entity_uuid":"e0","limit":3}`), sess)
		require.NoError(t, err)
		assert.Equal(t, 4, kg.gotLimit, "the graph must be asked for one entity past the limit")
		assert.Contains(t, res.Content, `"truncated":true`)
		assert.Contains(t, res.Content, `"count":3`)
	})
}
