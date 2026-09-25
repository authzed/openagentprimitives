package infoleakageaudit_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
)

func TestAuditKindRegistration(t *testing.T) {
	k, ok := memory.LookupKind("infoleakage_audit")
	require.True(t, ok)
	assert.Equal(t, "infoleakage_audit", k.Name())
	assert.Equal(t, "ila-", k.IDPrefix())

	ret := k.Retention()
	assert.Equal(t, 90*24*time.Hour, ret.TTLAfterArchive)
}

func TestAuditAppendList(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}

	require.NoError(t, infoleakageaudit.Append(ctx, m, scope,
		infoleakageaudit.AuditRecord{
			At:        time.Now().UTC().Truncate(time.Second),
			Kind:      "leakage_detected",
			Tool:      "linear.get_issue",
			Resources: []infoleakageaudit.ResourceRef{{Type: "linear_issue", ID: "ABC"}},
			LeakedTo:  []string{"user:marcy"},
			Requester: "user:alice",
		}))

	got, err := infoleakageaudit.List(ctx, m, scope)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "leakage_detected", got[0].Kind)
	assert.Equal(t, "linear.get_issue", got[0].Tool)
	require.Len(t, got[0].Resources, 1)
	assert.Equal(t, "linear_issue", got[0].Resources[0].Type)
	assert.Equal(t, []string{"user:marcy"}, got[0].LeakedTo)
}

func TestAuditScopeIsolation(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	s1 := memory.Scope{Kind: "session", ID: "ns/a"}
	s2 := memory.Scope{Kind: "session", ID: "ns/b"}

	require.NoError(t, infoleakageaudit.Append(ctx, m, s1,
		infoleakageaudit.AuditRecord{Kind: "x"}))
	require.NoError(t, infoleakageaudit.Append(ctx, m, s2,
		infoleakageaudit.AuditRecord{Kind: "y"}))

	gotA, err := infoleakageaudit.List(ctx, m, s1)
	require.NoError(t, err)
	gotB, err := infoleakageaudit.List(ctx, m, s2)
	require.NoError(t, err)

	require.Len(t, gotA, 1)
	require.Len(t, gotB, 1)
	assert.Equal(t, "x", gotA[0].Kind)
	assert.Equal(t, "y", gotB[0].Kind)
}
