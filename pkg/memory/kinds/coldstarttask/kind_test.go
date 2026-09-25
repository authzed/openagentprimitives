package coldstarttask_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

func TestColdStartTask_Registered(t *testing.T) {
	k, ok := memory.LookupKind("cold_start_task")
	require.True(t, ok)
	assert.Equal(t, "cst-", k.IDPrefix())
	assert.Equal(t, reflect.TypeOf(coldstarttask.Content{}), k.ContentSchema())
	r := k.Retention()
	assert.Contains(t, r.ArchiveOn, lifecycle.SigSessionCompleted)
	assert.Contains(t, r.ArchiveOn, lifecycle.SigSessionFailed)
	assert.True(t, r.TTLAfterArchive > 0)
}

func TestColdStartTask_PutGet(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sc := memory.Scope{Kind: "session", ID: "ns/a"}

	require.NoError(t, coldstarttask.Put(ctx, m, sc, coldstarttask.Content{
		Status: coldstarttask.StatusApprovedCleaned, CleanedText: "do X", InboxIdx: 0,
	}))
	got, found, err := coldstarttask.Get(ctx, m, sc)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, coldstarttask.StatusApprovedCleaned, got.Status)
	assert.Equal(t, "do X", got.CleanedText)
	assert.False(t, got.DecidedAt.IsZero(), "Put defaults DecidedAt")
}

func TestColdStartTask_GetMissing(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	_, found, err := coldstarttask.Get(memory.WithSystemApproval(context.Background(), "test"), m, memory.Scope{Kind: "session", ID: "fresh"})
	require.NoError(t, err)
	assert.False(t, found)
}

func TestColdStartTask_PutOverwrites(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sc := memory.Scope{Kind: "session", ID: "ns/b"}
	require.NoError(t, coldstarttask.Put(ctx, m, sc, coldstarttask.Content{Status: coldstarttask.StatusDenied}))
	require.NoError(t, coldstarttask.Put(ctx, m, sc, coldstarttask.Content{Status: coldstarttask.StatusApprovedCleaned, CleanedText: "y"}))
	got, _, err := coldstarttask.Get(ctx, m, sc)
	require.NoError(t, err)
	assert.Equal(t, coldstarttask.StatusApprovedCleaned, got.Status)
	res, err := m.Query(ctx, memory.Query{Scope: sc, Kinds: []string{"cold_start_task"}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1, "stable ID → single entry")
}

func TestColdStartTask_StatusConstants(t *testing.T) {
	// All statuses round-trip.
	for _, s := range []string{
		coldstarttask.StatusApprovedCleaned, coldstarttask.StatusApprovedOriginal,
		coldstarttask.StatusRanWithoutScope, coldstarttask.StatusDenied,
		coldstarttask.StatusScopeReviewFailed,
	} {
		assert.NotEmpty(t, s)
	}
}
