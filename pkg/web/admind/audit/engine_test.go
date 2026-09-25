package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/web/admind/audit"
)

func seedEngine(t *testing.T) *audit.Engine {
	t.Helper()
	b := inmem.NewBackend()
	mem := memory.NewLocal(b)
	ctx := context.Background()
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	puts := []memory.Entry{
		entry("approval", "approval-1", map[string]any{"toolName": "bash", "decision": "approved", "approver": "user:abc"}),
		entry("approval", "approval-2", map[string]any{"toolName": "rm", "decision": "denied", "approver": "user:abc"}),
		entry("authz_decision", "authzd-1", map[string]any{"outcome": "allowed", "subject": "user:abc", "permission": "read"}),
		entry("infoleakage_audit", "ila-1", map[string]any{"kind": "read_denied", "tool": "file_read", "requester": "user:abc"}),
	}
	// Spread sessions + times: approval-2 on a second session, later.
	puts[0].CreatedAt = t0
	puts[1].Scope.ID = "default/s2"
	puts[1].CreatedAt = t0.Add(time.Minute)
	puts[2].CreatedAt = t0.Add(2 * time.Minute)
	puts[3].CreatedAt = t0.Add(3 * time.Minute)
	for _, e := range puts {
		_, err := mem.Put(ctx, e)
		require.NoError(t, err)
	}
	return &audit.Engine{
		Mem:          mem,
		ResolveClass: func(_ context.Context, ns, name string) string { return "demo-class" },
		Logger:       testr.New(t),
	}
}

func TestEngineQuery_FiltersAndPaginates(t *testing.T) {
	g := seedEngine(t)
	ctx := context.Background()

	res, err := g.Query(ctx, audit.QueryRequest{})
	require.NoError(t, err)
	require.Len(t, res.Events, 4, "no filters = everything, newest first")
	assert.Equal(t, "leakage", res.Events[0].Kind, "ila-1 (t0+3m) is newest")
	assert.Equal(t, "demo-class", res.Events[0].AgentClass, "engine enriches AgentClass")

	res, err = g.Query(ctx, audit.QueryRequest{Outcome: "denied"})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	assert.Equal(t, "rm", res.Events[0].Tool)

	res, err = g.Query(ctx, audit.QueryRequest{SessionName: "s2"})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)

	res, err = g.Query(ctx, audit.QueryRequest{EventKinds: []string{"approval"}, Limit: 1})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	assert.True(t, res.HasMore)
	res, err = g.Query(ctx, audit.QueryRequest{EventKinds: []string{"approval"}, Limit: 1, Offset: 1})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	assert.False(t, res.HasMore)
}

func TestEngineFacets(t *testing.T) {
	g := seedEngine(t)
	f, err := g.Facets(context.Background(), audit.QueryRequest{})
	require.NoError(t, err)
	assert.Equal(t, 2, f.Counts["kind"]["approval"])
	assert.Equal(t, 1, f.Counts["kind"]["authz_decision"])
	assert.Equal(t, 1, f.Counts["kind"]["leakage"])
	assert.Equal(t, 1, f.Counts["outcome"]["denied"])
	assert.Equal(t, 1, f.Counts["outcome"]["read_denied"])
	assert.Equal(t, 4, f.Counts["agentClass"]["demo-class"])
}

func TestEngineEntities(t *testing.T) {
	g := seedEngine(t)
	rows, err := g.Entities(context.Background(), "sessions", audit.QueryRequest{})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	// Sorted by event count desc; default/s1 has 3 events (approval + authz + ila read_denied).
	assert.Equal(t, "default/s1", rows[0].Key)
	assert.Equal(t, 3, rows[0].Events)
	// s1 has one "denied" (approval-2 is on s2) and one "read_denied" (ila-1 on s1).
	// The read_denied must be counted in Denied.
	assert.Equal(t, 1, rows[0].Denied, "read_denied on s1 counts as Denied")
	assert.Equal(t, 1, rows[1].Denied, "denied outcome on s2 counts as Denied")
	assert.Equal(t, 2, rows[1].Denied+rows[0].Denied, "two denials total: one denied + one read_denied")

	_, err = g.Entities(context.Background(), "nope", audit.QueryRequest{})
	assert.Error(t, err, "unknown axis must error")
}
