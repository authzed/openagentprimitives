package meta_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

type fakeMemSearcher struct {
	result      memory.MergedSearchResult
	queryResult memory.QueryResult
	err         error
	lastReq     memory.SearchRequest
	lastQuery   memory.Query
}

func (m *fakeMemSearcher) Query(_ context.Context, q memory.Query) (memory.QueryResult, error) {
	m.lastQuery = q
	return m.queryResult, m.err
}

func (m *fakeMemSearcher) Search(_ context.Context, req memory.SearchRequest) (memory.MergedSearchResult, error) {
	m.lastReq = req
	return m.result, m.err
}

func TestSearchMemoryTool_Name(t *testing.T) {
	st := meta.NewSearchMemory()
	assert.Equal(t, "search_memory", st.Name())
}

func TestSearchMemoryTool_Execute(t *testing.T) {
	fm := &fakeMemSearcher{
		result: memory.MergedSearchResult{
			Entries: []memory.ScoredEntry{
				{Entry: memory.Entry{Kind: "entity", ID: "ent-1", Content: json.RawMessage(`{"name":"Bob"}`)}, Score: 0.9, Source: "pg"},
			},
		},
	}
	sess := &tool.SessionContext{
		Namespace: "default",
		Name:      "test-session",
		Mem:       fm,
	}
	st := meta.NewSearchMemory()
	res, err := st.Execute(memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"text":"find Bob","limit":5}`), sess)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	// Corrected: this used to assert Trusted (the blanket "framework meta tool"
	// sweep). Result.Trusted means framework-CONTROLLED content; a recall tool
	// relays whatever was stored, so it must stay untrusted and be inspected.
	// See TestMemoryRecallTools_ThirdPartyContent_IsUntrusted.
	assert.False(t, res.Trusted, "search_memory relays stored third-party content and must be inspected")

	require.Len(t, fm.lastReq.Scopes, 1)
	assert.Equal(t, "session", fm.lastReq.Scopes[0].Kind)
	assert.Equal(t, "default/test-session", fm.lastReq.Scopes[0].ID)
	assert.Equal(t, "find Bob", fm.lastReq.Text)
	assert.Equal(t, 5, fm.lastReq.Limit)

	var out map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(res.Content), &out))
	assert.Equal(t, float64(1), out["count"])
}

func TestSearchMemoryTool_FallbackOnNoProviders(t *testing.T) {
	fm := &fakeMemSearcher{err: memory.ErrNoSearchProviders}
	sess := &tool.SessionContext{
		Namespace: "default",
		Name:      "test-session",
		Mem:       fm,
	}
	st := meta.NewSearchMemory()
	res, err := st.Execute(memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"text":"anything"}`), sess)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "no search providers")
}

func TestSearchMemoryTool_AdditionalScopes(t *testing.T) {
	fm := &fakeMemSearcher{result: memory.MergedSearchResult{}}
	sess := &tool.SessionContext{
		Namespace: "default",
		Name:      "test-session",
		Mem:       fm,
	}
	st := meta.NewSearchMemory()
	_, err := st.Execute(memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"text":"test","scopes":["user/sam@example.com"]}`), sess)
	require.NoError(t, err)
	require.Len(t, fm.lastReq.Scopes, 2)
	assert.Equal(t, "default/test-session", fm.lastReq.Scopes[0].ID)
	assert.Equal(t, "user", fm.lastReq.Scopes[1].Kind)
	assert.Equal(t, "sam@example.com", fm.lastReq.Scopes[1].ID)
}

// TestSearchMemoryTool_DroppedFiltersFollowProviderNameOrder pins the CHOICE,
// which the determinism guard cannot state: the flattened list is grouped by
// provider in ascending provider name, keeping each provider's own ordering of
// its own filters. Sorting the flattened list instead would also be stable and
// would lose that grouping.
func TestSearchMemoryTool_DroppedFiltersFollowProviderNameOrder(t *testing.T) {
	fm := &fakeMemSearcher{result: memory.MergedSearchResult{
		PerProvider: map[string]memory.SearchResult{
			"zeta":  {DroppedFilters: []string{"z-second", "z-first"}},
			"alpha": {DroppedFilters: []string{"a-only"}},
			"mid":   {DroppedFilters: []string{"m-only"}},
		},
	}}
	st := meta.NewSearchMemory()
	res, err := st.Execute(context.Background(), json.RawMessage(`{"text":"q"}`),
		&tool.SessionContext{Namespace: "default", Name: "demo-session", Mem: fm})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)

	var got struct {
		DroppedFilters []string `json:"dropped_filters"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content), &got))
	assert.Equal(t, []string{"a-only", "m-only", "z-second", "z-first"}, got.DroppedFilters,
		"providers in ascending name order; each provider's own filter order preserved")
}

// TestSearchMemoryTool_EntriesCarryCreatedAt pins the one field that lets a
// reader order two entries against each other.
//
// search_memory ranks by RELEVANCE, and a caller deciding which of two
// contradicting notes is current — a pause and the resume meant to cancel it
// — cannot get that from a score. query_memory has always emitted created_at
// in this exact layout; this is the same fact through the tool that can also
// reach a resource pool, which is where two sessions' notes about one
// resource actually meet.
//
// The layout is asserted, not just the presence: it is UTC with no offset and
// fixed width, so a reader may order two of them as plain strings. A layout
// that varied in width or carried an offset would compare wrong exactly when
// the two notes straddled a zone change.
func TestSearchMemoryTool_EntriesCarryCreatedAt(t *testing.T) {
	when := time.Date(2026, 9, 16, 14, 5, 6, 0, time.UTC)
	fm := &fakeMemSearcher{
		result: memory.MergedSearchResult{
			Entries: []memory.ScoredEntry{{
				Entry: memory.Entry{
					Kind:      "observation",
					ID:        "obs-1",
					CreatedAt: when,
					Content:   json.RawMessage(`{"text":"reviews paused"}`),
				},
				Score:  0.9,
				Source: "inmem",
			}},
		},
	}
	sess := &tool.SessionContext{Namespace: "default", Name: "s", Mem: fm}
	res, err := meta.NewSearchMemory().Execute(
		memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"text":"pause","limit":5}`), sess)
	require.NoError(t, err)
	require.False(t, res.IsError)

	var out struct {
		Entries []struct {
			CreatedAt string `json:"created_at"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content), &out))
	require.Len(t, out.Entries, 1)
	assert.Equal(t, "2026-09-16T14:05:06Z", out.Entries[0].CreatedAt,
		"same layout query_memory emits, so the two tools' envelopes agree and either is string-sortable")
}

// Missing time bounds would let an old, highly ranked run displace the latest baseline.
func TestSearchMemoryTool_TimeWindow(t *testing.T) {
	fm := &fakeMemSearcher{}
	res, err := meta.NewSearchMemory().Execute(context.Background(), json.RawMessage(`{"since":"2026-10-01T00:00:00-04:00","until":"2026-10-08T04:00:00Z","tags":["coverage-success"]}`), &tool.SessionContext{Namespace: "default", Name: "coverage", Mem: fm})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)
	require.NotNil(t, fm.lastReq.Since)
	require.NotNil(t, fm.lastReq.Until)
	assert.True(t, fm.lastReq.Since.Equal(time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)))
	assert.True(t, fm.lastReq.Until.Equal(time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)))
	assert.Equal(t, []string{"coverage-success"}, fm.lastReq.Tags)
}

func TestSearchMemoryTool_InvalidTimeWindow(t *testing.T) {
	for _, tt := range []struct{ name, args, want string }{
		{"bad since", `{"since":"yesterday"}`, "since"},
		{"bad until", `{"until":"2026-10-01"}`, "until"},
		{"equal bounds", `{"since":"2026-10-01T00:00:00Z","until":"2026-10-01T00:00:00Z"}`, "before"},
		{"reversed bounds", `{"since":"2026-10-08T00:00:00Z","until":"2026-10-01T00:00:00Z"}`, "before"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fm := &fakeMemSearcher{}
			res, err := meta.NewSearchMemory().Execute(context.Background(), json.RawMessage(tt.args), &tool.SessionContext{Namespace: "default", Name: "coverage", Mem: fm})
			require.NoError(t, err)
			require.True(t, res.IsError, res.Content)
			assert.True(t, res.Trusted)
			assert.Contains(t, res.Content, tt.want)
			assert.Empty(t, fm.lastReq.Scopes, "invalid bounds must never reach the backend")
		})
	}
}

func TestSearchMemoryTool_ReportsIncompleteHistory(t *testing.T) {
	fm := &fakeMemSearcher{}
	require.NoError(t, json.Unmarshal([]byte(`{"partial":true,"failedProviders":["history"]}`), &fm.result))
	res, err := meta.NewSearchMemory().Execute(context.Background(), json.RawMessage(`{}`), &tool.SessionContext{Namespace: "default", Name: "run", Mem: fm})
	require.NoError(t, err)
	require.False(t, res.IsError)
	var got struct {
		Partial         bool     `json:"partial"`
		FailedProviders []string `json:"failed_providers"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content), &got))
	assert.True(t, got.Partial)
	assert.Equal(t, []string{"history"}, got.FailedProviders)
}
