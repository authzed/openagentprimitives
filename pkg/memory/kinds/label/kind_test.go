package label_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/label"
)

func TestLabel_Registered(t *testing.T) {
	k, ok := memory.LookupKind("label")
	require.True(t, ok)
	assert.Equal(t, "label-", k.IDPrefix())
	assert.Equal(t, 4096, k.Retention().SoftCapPerScope)
}

func TestLabel_RecordAndGet(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, label.Record(ctx, m, scope, "github_repo", "authzed/spicedb", "Authzed SpiceDB"))
	require.NoError(t, label.Record(ctx, m, scope, "github_repo", "authzed/spicedb", "Authzed SpiceDB v2"))

	got, ok, err := label.Get(ctx, m, scope, "github_repo", "authzed/spicedb")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "Authzed SpiceDB v2", got)
}

func TestLabel_TaggedUntrusted(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, label.Record(memory.WithSystemApproval(context.Background(), "test"), m, scope, "github_repo", "x/y", "X / Y"))

	res, err := m.Query(memory.WithSystemApproval(context.Background(), "test"), memory.Query{Scope: scope, Kinds: []string{"label"}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Contains(t, res.Entries[0].Tags, "trust:untrusted")
}
