package uiaction_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
)

// newMemory builds a fresh in-memory backend for a fixed session scope,
// mirroring pkg/memory/kinds/turn's accessor_test fixture shape.
func newMemory(t *testing.T) (memory.Memory, memory.Scope) {
	t.Helper()
	return memory.NewLocal(inmem.NewBackend()), memory.Scope{Kind: "session", ID: "demo-ns/demo-session"}
}

func TestRecord_ThenByRequestID_RoundTrips(t *testing.T) {
	m, scope := newMemory(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	in := uiaction.Content{
		RequestID: "req-1",
		Action:    "advance_stage",
		State:     uiaction.StateRunning,
		Requester: "alice",
		UpdatedAt: time.Now().UTC(),
	}
	require.NoError(t, uiaction.Record(ctx, m, scope, in))

	got, ok, err := uiaction.ByRequestID(ctx, m, scope, "req-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, in.RequestID, got.RequestID)
	assert.Equal(t, in.Action, got.Action)
	assert.Equal(t, uiaction.StateRunning, got.State)
	assert.Equal(t, "alice", got.Requester)
}

func TestByRequestID_Absent(t *testing.T) {
	m, scope := newMemory(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	_, ok, err := uiaction.ByRequestID(ctx, m, scope, "req-never-written")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestRecord_SecondWriteReplacesRatherThanAppends(t *testing.T) {
	m, scope := newMemory(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, uiaction.Record(ctx, m, scope, uiaction.Content{
		RequestID: "req-1", Action: "advance_stage", State: uiaction.StateSubmitted,
		Requester: "alice", UpdatedAt: time.Now().UTC(),
	}))
	require.NoError(t, uiaction.Record(ctx, m, scope, uiaction.Content{
		RequestID: "req-1", Action: "advance_stage", State: uiaction.StateSucceeded,
		Requester: "alice", UpdatedAt: time.Now().UTC(),
	}))

	got, ok, err := uiaction.ByRequestID(ctx, m, scope, "req-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uiaction.StateSucceeded, got.State, "the second write must win")

	all, err := uiaction.List(ctx, m, scope, "alice", 10)
	require.NoError(t, err)
	assert.Len(t, all, 1, "a second Record for the same request ID must replace, not append")
}

func TestList_FiltersByRequesterAndOrdersNewestFirst(t *testing.T) {
	m, scope := newMemory(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	base := time.Now().UTC()
	require.NoError(t, uiaction.Record(ctx, m, scope, uiaction.Content{
		RequestID: "req-1", Action: "advance_stage", State: uiaction.StateSucceeded,
		Requester: "alice", UpdatedAt: base,
	}))
	require.NoError(t, uiaction.Record(ctx, m, scope, uiaction.Content{
		RequestID: "req-2", Action: "advance_stage", State: uiaction.StateSucceeded,
		Requester: "alice", UpdatedAt: base.Add(time.Minute),
	}))
	require.NoError(t, uiaction.Record(ctx, m, scope, uiaction.Content{
		RequestID: "req-3", Action: "advance_stage", State: uiaction.StateSucceeded,
		Requester: "bob", UpdatedAt: base.Add(2 * time.Minute),
	}))

	got, err := uiaction.List(ctx, m, scope, "alice", 10)
	require.NoError(t, err)
	require.Len(t, got, 2, "bob's record must never come back for alice's requester filter")
	for _, c := range got {
		assert.Equal(t, "alice", c.Requester)
	}
	assert.Equal(t, "req-2", got[0].RequestID, "newest first")
	assert.Equal(t, "req-1", got[1].RequestID)
}

func TestList_RespectsLimitAfterFiltering(t *testing.T) {
	m, scope := newMemory(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	base := time.Now().UTC()
	for i, id := range []string{"req-1", "req-2", "req-3"} {
		require.NoError(t, uiaction.Record(ctx, m, scope, uiaction.Content{
			RequestID: id, Action: "advance_stage", State: uiaction.StateSucceeded,
			Requester: "alice", UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}))
	}

	got, err := uiaction.List(ctx, m, scope, "alice", 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "req-3", got[0].RequestID)
	assert.Equal(t, "req-2", got[1].RequestID)
}

func TestRequesterKey_PrefixedAndBareAgree(t *testing.T) {
	assert.Equal(t, uiaction.RequesterKey("user:abc"), uiaction.RequesterKey("abc"))
	assert.Equal(t, "abc", uiaction.RequesterKey("user:abc"))
	assert.Equal(t, "abc", uiaction.RequesterKey("abc"))
}

func TestEntryID_HasKindIDPrefix(t *testing.T) {
	k, ok := memory.LookupKind("ui_action")
	require.True(t, ok, "ui_action must be registered")
	assert.True(t, len(uiaction.EntryID("req-1")) > len(k.IDPrefix()))
	assert.Equal(t, k.IDPrefix()+"req-1", uiaction.EntryID("req-1"))
}

// TestKind_MutableRetention pins the retention decision this Kind exists to
// document: a ui_action record is rewritten in place as the lifecycle
// advances, so it must NOT be append-only, and it must never be evicted
// out from under a live session (a pending approval record disappearing
// mid-wait would strand the control permanently disabled).
func TestKind_MutableRetention(t *testing.T) {
	k, ok := memory.LookupKind("ui_action")
	require.True(t, ok)
	assert.Equal(t, "uiact-", k.IDPrefix())
	assert.False(t, k.Retention().AppendOnly,
		"ui_action is updated in place (submitted -> ... -> settled); append-only would reject every update after the first write")
	assert.True(t, k.Retention().EssentialWhileLive,
		"a live session must never evict a pending action record out from under an in-flight approval")
}

// TestKind_TTLAfterArchiveExceedsApprovalTimeout pins the mechanical answer
// to the design spec's "the ui_action TTL must exceed the resolved approval
// timeout": a session that completes while an approval is still pending must
// not let the record expire before the approval deadline can even fire.
func TestKind_TTLAfterArchiveExceedsApprovalTimeout(t *testing.T) {
	k, ok := memory.LookupKind("ui_action")
	require.True(t, ok)
	var nilBlock *spiceboxv1alpha1.AuthzBlock // nil AuthzBlock resolves to the 10-minute default
	assert.Greater(t, k.Retention().TTLAfterArchive, nilBlock.ResolvedApprovalTimeout(),
		"TTLAfterArchive must comfortably outlive the default resolved approval timeout")
}
