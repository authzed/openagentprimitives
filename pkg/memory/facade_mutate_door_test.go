package memory_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// TestDeleteDoor_Mutable exercises the DeleteMemory door on Delete for a
// mutable Kind: no approval denies before the backend is touched, a matching
// DeleteMemory (or system) approval allows.
func TestDeleteDoor_Mutable(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(fakeKind{name: "mutate-door-delete", prefix: "mdd-"})
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "nsA/sessA"}
	e := memory.Entry{
		Scope:     scope,
		Kind:      "mutate-door-delete",
		ID:        "mdd-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	}
	_, err := m.Put(memory.WithSystemApproval(context.Background(), "test"), e)
	require.NoError(t, err, "seed entry to delete")

	// No approval: denied before ever touching the backend.
	err = m.Delete(context.Background(), scope, e.Kind, e.ID)
	require.ErrorIs(t, err, memory.ErrMissingApproval)

	// Matching DeleteMemory approval: allowed.
	ctx := memory.WithApproval(context.Background(),
		memory.ForBearerToken(memory.DeleteMemory, scope.ID, "tok-1"))
	err = m.Delete(ctx, scope, e.Kind, e.ID)
	assert.NoError(t, err)

	// System approval: allowed (idempotent — already deleted).
	err = m.Delete(memory.WithSystemApproval(context.Background(), "test"), scope, e.Kind, e.ID)
	assert.NoError(t, err)
}

// TestDeleteDoor_AppendOnlyRefusalStaysFirst proves the pre-existing
// append-only refusal fires before the new DeleteMemory door: deleting an
// append-only Kind is refused even carrying a valid DeleteMemory approval.
func TestDeleteDoor_AppendOnlyRefusalStaysFirst(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendOnlyKind{name: "ao-mutate-door", prefix: "aomd-"})
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "nsA/sessA"}

	ctx := memory.WithApproval(context.Background(),
		memory.ForBearerToken(memory.DeleteMemory, scope.ID, "tok-1"))
	err := m.Delete(ctx, scope, "ao-mutate-door", "aomd-1")
	require.ErrorIs(t, err, memory.ErrAppendOnlyKind,
		"append-only refusal must fire before the approval door, even with a valid approval")
}

// TestDeleteScopeDoor exercises the DeleteMemory door on DeleteScope. Only
// system callers reach this in production (the controller injects a system
// approval on teardown), so there is no bearer/SpiceDB path to assert here.
func TestDeleteScopeDoor(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "nsA/sessA"}

	// No approval: denied.
	err := m.DeleteScope(context.Background(), scope)
	require.ErrorIs(t, err, memory.ErrMissingApproval)

	// System approval: allowed.
	err = m.DeleteScope(memory.WithSystemApproval(context.Background(), "test"), scope)
	assert.NoError(t, err)
}

// TestSendSignalDoor exercises the WriteMemory door on SendSignal.
func TestSendSignalDoor(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "nsA/sessA"}
	sig := memory.Signal{Kind: "test/ping", Scope: scope, At: time.Now().UTC()}

	// No approval: denied.
	err := m.SendSignal(context.Background(), sig)
	require.ErrorIs(t, err, memory.ErrMissingApproval)

	// Matching WriteMemory approval: allowed.
	ctx := memory.WithApproval(context.Background(),
		memory.ForBearerToken(memory.WriteMemory, scope.ID, "tok-1"))
	err = m.SendSignal(ctx, sig)
	assert.NoError(t, err)

	// System approval: allowed.
	err = m.SendSignal(memory.WithSystemApproval(context.Background(), "test"), sig)
	assert.NoError(t, err)
}

// TestReindexDoor exercises the ReadMemory door on Reindex, including the
// two-return-value error path.
func TestReindexDoor(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "nsA/sessA"}

	// No approval: denied, count is zero.
	count, err := m.Reindex(context.Background(), scope)
	require.ErrorIs(t, err, memory.ErrMissingApproval)
	assert.Equal(t, 0, count)

	// Matching ReadMemory approval: allowed.
	ctx := memory.WithApproval(context.Background(),
		memory.ForBearerToken(memory.ReadMemory, scope.ID, "tok-1"))
	count, err = m.Reindex(ctx, scope)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	// System approval: allowed.
	count, err = m.Reindex(memory.WithSystemApproval(context.Background(), "test"), scope)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}
