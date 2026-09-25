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

// TestDeleteScope_RetainsAppendOnly is the RC-1 regression: session teardown must
// NOT destroy the tamper-evident audit log. DeleteScope is STRUCTURALLY unable to
// remove append-only kinds (transcript, audit, authz-decision, tool-session, …) —
// it removes only mutable/working memory, so the audit is PERMANENT and survives
// every scope deletion. There is deliberately no method that can erase it.
func TestDeleteScope_RetainsAppendOnly(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendOnlyKind{name: "ao", prefix: "ao-"})
	memory.RegisterKind(fakeKind{name: "mutable", prefix: "m-"})
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")

	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	audit := memory.Entry{Scope: scope, Kind: "ao", ID: "ao-1", CreatedAt: time.Unix(0, 0).UTC(), Content: json.RawMessage(`{"v":1}`)}
	working := memory.Entry{Scope: scope, Kind: "mutable", ID: "m-1", CreatedAt: time.Unix(0, 0).UTC(), Content: json.RawMessage(`{"v":1}`)}
	_, err := mem.Put(ctx, audit)
	require.NoError(t, err, "put append-only audit entry")
	_, err = mem.Put(ctx, working)
	require.NoError(t, err, "put ephemeral working entry")

	require.NoError(t, mem.DeleteScope(ctx, scope), "scope deletion must succeed")

	_, ok, err := mem.Get(ctx, scope, "ao", "ao-1")
	require.NoError(t, err)
	assert.True(t, ok, "append-only (audit) entry MUST survive ephemeral cleanup")

	_, ok, err = mem.Get(ctx, scope, "mutable", "m-1")
	require.NoError(t, err)
	assert.False(t, ok, "ephemeral (mutable) entry must be deleted")
}

// TestDeleteScope_AuditOnlyScopeIsNoop: DeleteScope on a scope with nothing
// ephemeral (an audit-only or already-clean scope) is a no-op that preserves the
// append-only entries.
func TestDeleteScope_AuditOnlyScopeIsNoop(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendOnlyKind{name: "ao", prefix: "ao-"})
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")

	scope := memory.Scope{Kind: "session", ID: "ns/b"}
	audit := memory.Entry{Scope: scope, Kind: "ao", ID: "ao-1", CreatedAt: time.Unix(0, 0).UTC(), Content: json.RawMessage(`{"v":1}`)}
	_, err := mem.Put(ctx, audit)
	require.NoError(t, err)

	require.NoError(t, mem.DeleteScope(ctx, scope))

	_, ok, err := mem.Get(ctx, scope, "ao", "ao-1")
	require.NoError(t, err)
	assert.True(t, ok, "audit-only scope: entry preserved, no-op cleanup")
}
