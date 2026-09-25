package infoleakagedecision_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagedecision"
)

func TestDecisionKindRegistration(t *testing.T) {
	k, ok := memory.LookupKind("infoleakage_decision")
	require.True(t, ok)
	assert.Equal(t, "infoleakage_decision", k.Name())
	assert.Equal(t, "ild-", k.IDPrefix())
}

func TestDecisionRecordListRoundTrip(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/sess1"}

	require.NoError(t, infoleakagedecision.Record(ctx, m, scope,
		infoleakagedecision.DecisionApproved, "issue", "ENG-1"))

	got, err := infoleakagedecision.List(ctx, m, scope)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "issue", got[0].ResourceType)
	assert.Equal(t, "ENG-1", got[0].ResourceID)
	assert.Equal(t, infoleakagedecision.DecisionApproved, got[0].Decision)
	assert.False(t, got[0].At.IsZero(), "At must be stamped")
}

func TestDecisionIsApprovedIsDenied(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/sess1"}

	require.NoError(t, infoleakagedecision.Record(ctx, m, scope,
		infoleakagedecision.DecisionApproved, "issue", "ENG-1"))
	require.NoError(t, infoleakagedecision.Record(ctx, m, scope,
		infoleakagedecision.DecisionDenied, "issue", "ENG-2"))

	approved, err := infoleakagedecision.IsApproved(ctx, m, scope, "issue", "ENG-1")
	require.NoError(t, err)
	assert.True(t, approved, "ENG-1 was approved")

	denied, err := infoleakagedecision.IsDenied(ctx, m, scope, "issue", "ENG-2")
	require.NoError(t, err)
	assert.True(t, denied, "ENG-2 was denied")

	// Cross checks: ENG-1 is not denied; ENG-2 is not approved.
	noDeny, err := infoleakagedecision.IsDenied(ctx, m, scope, "issue", "ENG-1")
	require.NoError(t, err)
	assert.False(t, noDeny)
	noApprove, err := infoleakagedecision.IsApproved(ctx, m, scope, "issue", "ENG-2")
	require.NoError(t, err)
	assert.False(t, noApprove)

	// Unknown resource is neither.
	unknownApproved, err := infoleakagedecision.IsApproved(ctx, m, scope, "issue", "ENG-999")
	require.NoError(t, err)
	assert.False(t, unknownApproved)
	unknownDenied, err := infoleakagedecision.IsDenied(ctx, m, scope, "issue", "ENG-999")
	require.NoError(t, err)
	assert.False(t, unknownDenied)
}

// TestDecisionSurvivesFreshAccessor is the restart-sim bar: a decision recorded
// through one accessor is readable through a fresh read over the SAME backend.
// The memory kind has no in-process state of its own, so this proves durability
// across a runner restart.
func TestDecisionSurvivesFreshAccessor(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	backend := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/sess1"}

	// "Before restart": record through one memory handle.
	m1 := memory.NewLocal(backend)
	require.NoError(t, infoleakagedecision.Record(ctx, m1, scope,
		infoleakagedecision.DecisionDenied, "issue", "ENG-1"))
	require.NoError(t, infoleakagedecision.Record(ctx, m1, scope,
		infoleakagedecision.DecisionApproved, "issue", "ENG-2"))

	// "After restart": a fresh memory handle over the SAME backend.
	m2 := memory.NewLocal(backend)
	denied, err := infoleakagedecision.IsDenied(ctx, m2, scope, "issue", "ENG-1")
	require.NoError(t, err)
	assert.True(t, denied, "denied decision must survive a fresh accessor (restart sim)")
	approved, err := infoleakagedecision.IsApproved(ctx, m2, scope, "issue", "ENG-2")
	require.NoError(t, err)
	assert.True(t, approved, "approved decision must survive a fresh accessor (restart sim)")
}

// TestDecisionIdempotentRerecord verifies re-recording the same tuple is
// harmless: IsApproved/IsDenied stay consistent (no duplicate-row corruption).
func TestDecisionIdempotentRerecord(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/sess1"}

	require.NoError(t, infoleakagedecision.Record(ctx, m, scope,
		infoleakagedecision.DecisionApproved, "issue", "ENG-1"))
	require.NoError(t, infoleakagedecision.Record(ctx, m, scope,
		infoleakagedecision.DecisionApproved, "issue", "ENG-1"))

	approved, err := infoleakagedecision.IsApproved(ctx, m, scope, "issue", "ENG-1")
	require.NoError(t, err)
	assert.True(t, approved, "re-recording the same approval is harmless")
}

// recordingMemory captures the Query a read issues, and answers with a store
// that honors field predicates the way the durable backends do.
type recordingMemory struct {
	entries []memory.Entry
	queries []memory.Query
}

func (r *recordingMemory) Put(_ context.Context, e memory.Entry) (memory.Entry, error) {
	r.entries = append(r.entries, e)
	return e, nil
}

func (r *recordingMemory) Query(_ context.Context, q memory.Query) (memory.QueryResult, error) {
	r.queries = append(r.queries, q)
	var out []memory.Entry
	for _, e := range r.entries {
		if matchesFields(e, q.FieldEquals) {
			out = append(out, e)
		}
	}
	return memory.QueryResult{Entries: out}, nil
}

func (r *recordingMemory) Search(_ context.Context, _ memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}
func (r *recordingMemory) SendSignal(_ context.Context, _ memory.Signal) error { return nil }

func matchesFields(e memory.Entry, filters []memory.FieldFilter) bool {
	if len(filters) == 0 {
		return true
	}
	var content map[string]any
	if err := json.Unmarshal(e.Content, &content); err != nil {
		return false
	}
	for _, f := range filters {
		if fmt.Sprint(content[f.Path]) != fmt.Sprint(f.Value) {
			return false
		}
	}
	return true
}

// Every leakage probe read the WHOLE kind and filtered in Go — over HTTP from
// the runner, once per tainted resource, on every reply. The predicates that
// select the one record belong in the query, so a backend that indexes content
// answers with one row instead of the session's entire decision history.
func TestDecisionProbePushesPredicatesIntoTheQuery(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := &recordingMemory{}
	scope := memory.Scope{Kind: "session", ID: "ns/sess1"}

	require.NoError(t, infoleakagedecision.Record(ctx, m, scope,
		infoleakagedecision.DecisionApproved, "issue", "ENG-1"))
	m.queries = nil

	approved, err := infoleakagedecision.IsApproved(ctx, m, scope, "issue", "ENG-1")
	require.NoError(t, err)
	assert.True(t, approved)

	require.Len(t, m.queries, 1)
	q := m.queries[0]
	assert.Equal(t, []string{"infoleakage_decision"}, q.Kinds)
	assert.ElementsMatch(t, []memory.FieldFilter{
		{Path: "resourceType", Value: "issue"},
		{Path: "resourceID", Value: "ENG-1"},
		{Path: "decision", Value: infoleakagedecision.DecisionApproved},
	}, q.FieldEquals, "the probe must ask for the record it wants")
	assert.Zero(t, q.Limit,
		"no Limit: a backend that DROPS the predicates would otherwise truncate the "+
			"unfiltered kind and turn a recorded decision into a false negative")
}
