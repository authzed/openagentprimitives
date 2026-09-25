package memory_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

func TestQueryDoor(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	q := memory.Query{Scope: memory.Scope{Kind: "session", ID: "nsA/sessA"}, Kinds: []string{"label"}}

	// No approval: denied.
	_, err := m.Query(context.Background(), q)
	require.ErrorIs(t, err, memory.ErrMissingApproval)

	// Matching bearer approval: allowed.
	ctx := memory.WithApproval(context.Background(),
		memory.ForBearerToken(memory.ReadMemory, "nsA/sessA", "tok-1"))
	_, err = m.Query(ctx, q)
	assert.NoError(t, err)

	// Bearer approval for a DIFFERENT scope: denied (cross-session).
	ctxB := memory.WithApproval(context.Background(),
		memory.ForBearerToken(memory.ReadMemory, "nsB/sessB", "tok-2"))
	_, err = m.Query(ctxB, q)
	require.ErrorIs(t, err, memory.ErrMissingApproval)

	// System approval: allowed.
	_, err = m.Query(memory.WithSystemApproval(context.Background(), "operator:test"), q)
	assert.NoError(t, err)
}
