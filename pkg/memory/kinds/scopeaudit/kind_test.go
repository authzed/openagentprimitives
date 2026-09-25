package scopeaudit_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/scopeaudit"
)

func TestScopeAudit_Registered(t *testing.T) {
	k, ok := memory.LookupKind("scope_audit")
	require.True(t, ok)
	assert.Equal(t, "saud-", k.IDPrefix())
	assert.Equal(t, reflect.TypeOf(scopeaudit.Content{}), k.ContentSchema())
}

func TestScopeAudit_RecordAndList(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sc := memory.Scope{Kind: "session", ID: "ns/a"}

	require.NoError(t, scopeaudit.Record(ctx, m, sc, scopeaudit.Content{
		Delta:     scope.ScopeDelta{Add: scope.ScopePartial{Tools: []string{"github.list_issues"}}},
		AppliedAt: time.Now(),
		Source:    "metaagent-approved",
		Approver:  "user:carl",
		Requester: "user:alice",
	}))

	records, err := scopeaudit.List(ctx, m, sc)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "user:alice", records[0].Requester)
	assert.Equal(t, "user:carl", records[0].Approver)
	assert.Equal(t, "metaagent-approved", records[0].Source)
}

func TestScopeAudit_EmptyListReturnsEmpty(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	sc := memory.Scope{Kind: "session", ID: "fresh"}
	records, err := scopeaudit.List(memory.WithSystemApproval(context.Background(), "test"), m, sc)
	require.NoError(t, err)
	assert.Empty(t, records, "empty list, not nil")
	assert.NotNil(t, records)
}

func TestScopeAudit_AutoAppliedAt(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sc := memory.Scope{Kind: "session", ID: "ns/a"}
	// AppliedAt zero → auto-set to now.
	require.NoError(t, scopeaudit.Record(ctx, m, sc, scopeaudit.Content{Source: "x"}))
	records, err := scopeaudit.List(ctx, m, sc)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.False(t, records[0].AppliedAt.IsZero())
}
