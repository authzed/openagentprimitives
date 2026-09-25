package runner_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/label"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

func TestMemoryFramework_E2EShape(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	lifecycle.Setup(m)
	defer lifecycle.Teardown()
	scope := memory.Scope{Kind: "session", ID: "ns/e2e"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Distinct timestamps: the lifecycle Timeline orders by createdAt,
	// and the backend's sort is not stable — equal times (the Signal
	// zero value) would make started-vs-completed order nondeterministic.
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, m.SendSignal(ctx, memory.Signal{Kind: lifecycle.SigSessionStarted, Scope: scope, At: t0}))

	require.NoError(t, authzdecision.Record(ctx, m, scope, "tu-1", authzdecision.Decision{
		Outcome: "allowed", Subject: "alice@example.com",
		ResourceType: "github_repo", ResourceID: "authzed/spicedb",
		Permission: "read",
	}))

	require.NoError(t, label.Record(ctx, m, scope, "github_repo", "authzed/spicedb", "Authzed SpiceDB"))

	require.NoError(t, m.SendSignal(ctx, memory.Signal{Kind: lifecycle.SigSessionCompleted, Scope: scope, At: t0.Add(time.Hour)}))

	timeline, err := lifecycle.Timeline(ctx, m, scope)
	require.NoError(t, err)
	require.Len(t, timeline, 2)
	assert.Contains(t, timeline[0].Tags, lifecycle.SignalTag(lifecycle.SigSessionStarted))
	assert.Contains(t, timeline[1].Tags, lifecycle.SignalTag(lifecycle.SigSessionCompleted))

	authzRes, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"authz_decision"}})
	require.NoError(t, err)
	assert.Len(t, authzRes.Entries, 1)

	labelRes, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"label"}})
	require.NoError(t, err)
	require.Len(t, labelRes.Entries, 1)
	assert.Contains(t, labelRes.Entries[0].Tags, "trust:untrusted")

	st, err := m.Status(ctx, scope)
	require.NoError(t, err)
	assert.Equal(t, memory.StatusLive, st.State)
}
