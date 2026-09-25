package meta_test

// A search may now span several memory pools at once: the session's own scope
// plus every resource pool the session reached through a slot grant. Which pool
// an entry came from is the fact the whole read path turns on — it decides the
// audience the entry's per-datum tag is minted against — and until this file it
// was dropped on the floor between memory.Entry and the tool result.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func poolScope(t *testing.T, objType, objID string) memory.Scope {
	t.Helper()
	s, err := memory.ResourceScope(objType, objID)
	require.NoError(t, err)
	return s
}

// searchAcross runs the tool over a result whose entries came from the given
// scopes, one entry each unless a scope repeats.
func searchAcross(t *testing.T, scopes ...memory.Scope) string {
	t.Helper()
	var entries []memory.ScoredEntry
	for i, sc := range scopes {
		entries = append(entries, memory.ScoredEntry{
			Entry: memory.Entry{
				Scope:   sc,
				Kind:    "observation",
				ID:      "obs-" + string(rune('a'+i)),
				Content: json.RawMessage(`{"note":"n` + string(rune('a'+i)) + `"}`),
			},
			Score:  1.0,
			Source: "inmem",
		})
	}
	fm := &fakeMemSearcher{result: memory.MergedSearchResult{Entries: entries}}
	sess := &tool.SessionContext{Namespace: "default", Name: "s1", Mem: fm}
	res, err := meta.NewSearchMemory().Execute(
		memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"text":"anything"}`), sess)
	require.NoError(t, err)
	require.False(t, res.IsError, "result: %s", res.Content)
	return res.Content
}

// TestSearchMemory_EveryEntryReportsTheScopeItCameFrom asserts the field is
// emitted for EVERY entry, session-scoped ones included.
//
// An envelope where the field's ABSENCE means "this session" is one a later
// reader gets wrong exactly once — and the reader here is the tagging path,
// which would then hand a pool's entry the session's own audience.
func TestSearchMemory_EveryEntryReportsTheScopeItCameFrom(t *testing.T) {
	sessionScope := memory.Scope{Kind: "session", ID: "default/s1"}
	body := searchAcross(t, sessionScope, poolScope(t, "customer", "alpha"))

	var out struct {
		Entries []struct {
			ID    string       `json:"id"`
			Scope memory.Scope `json:"scope"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.Len(t, out.Entries, 2)
	assert.Equal(t, sessionScope, out.Entries[0].Scope,
		"a session-scoped entry names its scope too; absence must not be the encoding for it")
	assert.Equal(t, poolScope(t, "customer", "alpha"), out.Entries[1].Scope)

	// The raw bytes, because the model reads these and a later reader parses
	// them: the scope is a nested object, not a string needing a split.
	assert.Contains(t, body, `"scope":{"kind":"resource","id":"customer:alpha"}`)
}

// TestSplitSearchResultByScope_GroupsEachPoolsEntriesTogether is the input the
// per-datum tagging needs: one group per pool, carrying ONLY that pool's
// entries. A group carrying the whole result would put one pool's entries
// behind another pool's audience.
func TestSplitSearchResultByScope_GroupsEachPoolsEntriesTogether(t *testing.T) {
	alpha := poolScope(t, "customer", "alpha")
	beta := poolScope(t, "customer", "beta")
	body := searchAcross(t, alpha, beta, alpha)

	groups, err := meta.SplitSearchResultByScope(body)
	require.NoError(t, err)
	require.Len(t, groups, 2, "one group per SCOPE, not per entry")

	assert.Equal(t, alpha, groups[0].Scope, "groups follow first appearance in the ranking")
	assert.Equal(t, beta, groups[1].Scope)

	var alphaOut struct {
		Entries []struct {
			ID string `json:"id"`
		} `json:"entries"`
		Count int `json:"count"`
	}
	require.NoError(t, json.Unmarshal([]byte(groups[0].Result), &alphaOut))
	assert.Equal(t, 2, alphaOut.Count, "count describes the group, not the whole search")
	require.Len(t, alphaOut.Entries, 2)
	assert.Equal(t, "obs-a", alphaOut.Entries[0].ID)
	assert.Equal(t, "obs-c", alphaOut.Entries[1].ID)

	assert.NotContains(t, groups[0].Result, "obs-b", "alpha's slice must not carry beta's entry")
	assert.NotContains(t, groups[1].Result, "obs-a", "beta's slice must not carry alpha's entry")
}

// TestSplitSearchResultByScope_RefusesWhatItCannotRead: an unreadable envelope
// is an ERROR, never an empty group list. "No pools" and "I could not tell" must
// not be the same answer — the caller treats the first as nothing-to-tag.
func TestSplitSearchResultByScope_RefusesWhatItCannotRead(t *testing.T) {
	_, err := meta.SplitSearchResultByScope("not json at all")
	require.Error(t, err)

	groups, err := meta.SplitSearchResultByScope(`{"entries":[],"count":0}`)
	require.NoError(t, err)
	assert.Empty(t, groups, "an empty result is readable and names no scope")
}

// The two recall tools differ in reach, and a description is the ONLY place an
// agent can learn that: search spans the session's resource pools, query does
// not. An agent holding both and told nothing would read a query result as the
// whole picture. Descriptions are prompt, so the claims are pinned.
func TestMemoryToolDescriptions_TellTheAgentWhichToolReachesPools(t *testing.T) {
	search := meta.NewSearchMemory().Description()
	assert.Contains(t, search, "resource pool",
		"search_memory must say its results may come from pools this session has access to")
	assert.Contains(t, search, "scope",
		"...and that every entry names the scope it came from")

	query := meta.NewQueryMemory().Description()
	assert.Contains(t, query, "search_memory",
		"query_memory must point at the tool that does reach pools")
}

// TestSearchMemoryDescription_DoesNotPromiseWhatTheServedPathIgnores corrects a
// standing defect rather than describing new behaviour.
//
// The description told the agent to "pass `scopes` to search additional
// sessions". On the served path the handler overwrites the request's scopes
// from the URL session plus that session's pools, and always did — so the
// argument has never done anything for the one caller that reads this text. The
// schema field stays (in-process callers honour it); the promise does not.
func TestSearchMemoryDescription_DoesNotPromiseWhatTheServedPathIgnores(t *testing.T) {
	st := meta.NewSearchMemory()
	assert.NotContains(t, st.Description(), "Pass `scopes`")

	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(st.InputSchema(), &schema))
	scopes, ok := schema.Properties["scopes"]
	require.True(t, ok, "the field stays: removing it is a wire change, and in-process callers honour it")
	assert.NotContains(t, scopes.Description, "Additional scopes to search",
		"the property must not repeat the promise either — it is the same text to the same reader")
}
