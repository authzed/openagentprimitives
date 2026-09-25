package overview

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// fakeMem is a scripted CrossScopeQuerier. It filters by Kind + Since,
// orders newest-first, paginates via Offset/Limit, and counts calls so the
// cache test can assert "no recompute".
type fakeMem struct {
	entries []memory.Entry
	calls   int
}

func (f *fakeMem) QueryAllScopes(_ context.Context, q memory.CrossScopeQuery) (memory.QueryResult, error) {
	f.calls++
	kinds := map[string]bool{}
	for _, k := range q.Kinds {
		kinds[k] = true
	}
	var matched []memory.Entry
	for _, e := range f.entries {
		if !kinds[e.Kind] {
			continue
		}
		if q.Since != nil && e.CreatedAt.Before(*q.Since) {
			continue
		}
		matched = append(matched, e)
	}
	sort.Slice(matched, func(i, j int) bool {
		if q.OrderDesc {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].CreatedAt.Before(matched[j].CreatedAt)
	})
	if q.Offset >= len(matched) {
		return memory.QueryResult{}, nil
	}
	matched = matched[q.Offset:]
	if q.Limit > 0 && len(matched) > q.Limit {
		matched = matched[:q.Limit]
	}
	return memory.QueryResult{Entries: matched}, nil
}

func turnEntry(t *testing.T, ns, name string, at time.Time, in, out int64, toolCalls int) memory.Entry {
	t.Helper()
	turn := memory.Turn{
		Index:     0,
		Role:      "assistant",
		CreatedAt: at,
		Usage:     &memory.Usage{InputTokens: in, OutputTokens: out},
	}
	for i := 0; i < toolCalls; i++ {
		turn.Content = append(turn.Content, memory.ContentBlock{
			Type:    "tool_use",
			ToolUse: &memory.ToolUseBlock{ID: "tu", Name: "do_thing"},
		})
	}
	b, err := json.Marshal(turn)
	require.NoError(t, err)
	return memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: ns + "/" + name},
		Kind:      "turn",
		ID:        "turn-" + at.Format(time.RFC3339Nano),
		CreatedAt: at,
		Content:   b,
	}
}

func authzEntry(t *testing.T, ns, name string, at time.Time, outcome string) memory.Entry {
	t.Helper()
	b, err := json.Marshal(map[string]string{"outcome": outcome, "subject": "user:alice"})
	require.NoError(t, err)
	return memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: ns + "/" + name},
		Kind:      "authz_decision",
		ID:        "az-" + at.Format(time.RFC3339Nano) + "-" + outcome,
		CreatedAt: at,
		Content:   b,
	}
}

func findModel(rows []ModelTokens, model string) (ModelTokens, bool) {
	for _, r := range rows {
		if r.Model == model {
			return r, true
		}
	}
	return ModelTokens{}, false
}

func findClass(rows []ClassTokens, class string) (ClassTokens, bool) {
	for _, r := range rows {
		if r.Class == class {
			return r, true
		}
	}
	return ClassTokens{}, false
}

func TestOverviewAggregates(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 30, 0, 0, time.UTC)
	// currentHour = 12:00; windowStart = 06-29 13:00. Bucket 23 = current hour.
	turnNow := time.Date(2026, 6, 30, 12, 10, 0, 0, time.UTC)     // idx 23
	turnEarlier := time.Date(2026, 6, 30, 10, 15, 0, 0, time.UTC) // idx 21

	mem := &fakeMem{entries: []memory.Entry{
		turnEntry(t, "default", "s1", turnNow, 40, 20, 1),
		turnEntry(t, "default", "s2", turnEarlier, 15, 5, 2),
		authzEntry(t, "default", "s1", time.Date(2026, 6, 30, 11, 0, 0, 0, time.UTC), "denied"),
		authzEntry(t, "default", "s2", time.Date(2026, 6, 30, 11, 5, 0, 0, time.UTC), "allowed"),
	}}

	snapshot := func() []LiveSession {
		return []LiveSession{
			{Model: "claude-opus-4-8", Class: "researcher", Active: true, InputTokens: 100, OutputTokens: 50, PendingApprovals: 2},
			{Model: "claude-sonnet-4-6", Class: "writer", Active: false, InputTokens: 200, OutputTokens: 80, PendingApprovals: 1},
			{Model: "claude-opus-4-8", Class: "researcher", Active: true, InputTokens: 30, OutputTokens: 10},
		}
	}

	eng := &Engine{
		Mem:      mem,
		Snapshot: snapshot,
		Logger:   logr.Discard(),
		Now:      func() time.Time { return now },
	}

	ov, err := eng.Overview(context.Background())
	require.NoError(t, err, "Overview must succeed")
	require.False(t, ov.Truncated, "no truncation on a tiny scan")

	// --- byModel sums ---
	opus, ok := findModel(ov.ByModel, "claude-opus-4-8")
	require.True(t, ok, "opus row present")
	assert.Equal(t, int64(130), opus.InputTokens)
	assert.Equal(t, int64(60), opus.OutputTokens)
	assert.Equal(t, 2, opus.Sessions)

	sonnet, ok := findModel(ov.ByModel, "claude-sonnet-4-6")
	require.True(t, ok, "sonnet row present")
	assert.Equal(t, int64(200), sonnet.InputTokens)
	assert.Equal(t, int64(80), sonnet.OutputTokens)
	assert.Equal(t, 1, sonnet.Sessions)

	// --- byAgentClass sums ---
	res, ok := findClass(ov.ByAgentClass, "researcher")
	require.True(t, ok, "researcher row present")
	assert.Equal(t, int64(130), res.InputTokens)
	assert.Equal(t, int64(60), res.OutputTokens)
	assert.Equal(t, 2, res.Sessions)

	wr, ok := findClass(ov.ByAgentClass, "writer")
	require.True(t, ok, "writer row present")
	assert.Equal(t, int64(200), wr.InputTokens)
	assert.Equal(t, 1, wr.Sessions)

	// --- hourly bucketing: entries in two different hours land in two buckets ---
	assert.Equal(t, int64(40), ov.Series24h[23].InputTokens, "current hour input")
	assert.Equal(t, int64(20), ov.Series24h[23].OutputTokens, "current hour output")
	assert.Equal(t, int64(15), ov.Series24h[21].InputTokens, "earlier hour input")
	assert.Equal(t, int64(5), ov.Series24h[21].OutputTokens, "earlier hour output")
	assert.Equal(t, int64(0), ov.Series24h[0].InputTokens, "empty bucket stays zero")

	// bucket hour-start stamps are contiguous, oldest first.
	windowStart := time.Date(2026, 6, 29, 13, 0, 0, 0, time.UTC)
	assert.Equal(t, windowStart.Unix(), ov.Series24h[0].HourStartUnix)
	assert.Equal(t, windowStart.Add(23*time.Hour).Unix(), ov.Series24h[23].HourStartUnix)

	// --- KPI math ---
	assert.Equal(t, 2, ov.KPIs.ActiveSessions, "two active live sessions")
	assert.Equal(t, int64(80), ov.KPIs.TokensToday, "40+20+15+5 today")
	assert.Equal(t, 3, ov.KPIs.ToolCalls24h, "1+2 tool_use blocks in window")
	assert.Equal(t, 1, ov.KPIs.Denials24h, "one denied authz_decision in window")
	assert.Equal(t, 3, ov.KPIs.ApprovalsPending, "2+1 pending approvals summed across live sessions")
}

// TestOverviewAggregates_ByModelBuckets proves the byModel rollup attributes
// tokens to the ACTUALLY-served model, not the session's blended configured
// model: an OpenRouter-style session with two ByModel buckets contributes a
// row per bucket (never a row for its own blended Model), while a direct
// session with no buckets falls back to today's behavior. No double-counting:
// the routed session's own Model/InputTokens/OutputTokens must not ALSO
// appear as a third row.
func TestOverviewAggregates_ByModelBuckets(t *testing.T) {
	mem := &fakeMem{}
	snapshot := func() []LiveSession {
		return []LiveSession{
			{
				Model: "openrouter/auto", Class: "router-bot", Active: true,
				InputTokens: 300, OutputTokens: 120,
				ByModel: []ModelBucket{
					{Model: "openrouter/anthropic/claude-3.5-sonnet", InputTokens: 200, OutputTokens: 80},
					{Model: "openrouter/openai/gpt-5", InputTokens: 100, OutputTokens: 40},
				},
			},
			{
				Model: "anthropic/claude-opus-4-8", Class: "direct-bot", Active: false,
				InputTokens: 500, OutputTokens: 150,
			},
		}
	}
	eng := &Engine{
		Mem:      mem,
		Snapshot: snapshot,
		Logger:   logr.Discard(),
		Now:      func() time.Time { return time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC) },
	}

	ov, err := eng.Overview(context.Background())
	require.NoError(t, err)

	require.Len(t, ov.ByModel, 3,
		"2 served-model buckets + 1 direct-session row = 3, never a 4th row for the blended openrouter/auto")

	sonnet, ok := findModel(ov.ByModel, "openrouter/anthropic/claude-3.5-sonnet")
	require.True(t, ok, "first served-model bucket present")
	assert.Equal(t, int64(200), sonnet.InputTokens)
	assert.Equal(t, int64(80), sonnet.OutputTokens)
	assert.Equal(t, 1, sonnet.Sessions)

	gptRow, ok := findModel(ov.ByModel, "openrouter/openai/gpt-5")
	require.True(t, ok, "second served-model bucket present")
	assert.Equal(t, int64(100), gptRow.InputTokens)
	assert.Equal(t, int64(40), gptRow.OutputTokens)

	_, ok = findModel(ov.ByModel, "openrouter/auto")
	assert.False(t, ok, "blended configured-model row must NOT appear when buckets are present")

	direct, ok := findModel(ov.ByModel, "anthropic/claude-opus-4-8")
	require.True(t, ok, "session with no buckets falls back to its own Model")
	assert.Equal(t, int64(500), direct.InputTokens)
	assert.Equal(t, int64(150), direct.OutputTokens)

	var totalIn, totalOut int64
	for _, r := range ov.ByModel {
		totalIn += r.InputTokens
		totalOut += r.OutputTokens
	}
	assert.Equal(t, int64(800), totalIn, "300+500 across all rows: no double-counting")
	assert.Equal(t, int64(270), totalOut, "120+150 across all rows: no double-counting")
}

func TestOverviewCachesWithinTTL(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 30, 0, 0, time.UTC)
	mem := &fakeMem{entries: []memory.Entry{
		turnEntry(t, "default", "s1", now.Add(-10*time.Minute), 10, 5, 0),
	}}
	eng := &Engine{
		Mem:      mem,
		Snapshot: func() []LiveSession { return nil },
		Logger:   logr.Discard(),
		Now:      func() time.Time { return now },
		TTL:      10 * time.Second,
	}

	first, err := eng.Overview(context.Background())
	require.NoError(t, err)
	callsAfterFirst := mem.calls
	require.GreaterOrEqual(t, callsAfterFirst, 1, "first call computes (hits memory)")

	second, err := eng.Overview(context.Background())
	require.NoError(t, err)
	assert.Equal(t, callsAfterFirst, mem.calls, "second call within TTL must NOT recompute")
	assert.Equal(t, first, second, "cached result is returned verbatim")
}

func TestOverviewRecomputesAfterTTL(t *testing.T) {
	cur := time.Date(2026, 6, 30, 12, 30, 0, 0, time.UTC)
	mem := &fakeMem{entries: []memory.Entry{
		turnEntry(t, "default", "s1", cur.Add(-10*time.Minute), 10, 5, 0),
	}}
	eng := &Engine{
		Mem:      mem,
		Snapshot: func() []LiveSession { return nil },
		Logger:   logr.Discard(),
		Now:      func() time.Time { return cur },
		TTL:      10 * time.Second,
	}
	_, err := eng.Overview(context.Background())
	require.NoError(t, err)
	callsAfterFirst := mem.calls

	cur = cur.Add(11 * time.Second) // past the TTL
	_, err = eng.Overview(context.Background())
	require.NoError(t, err)
	assert.Greater(t, mem.calls, callsAfterFirst, "call after TTL must recompute")
}
