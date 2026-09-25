package preferenceaccess

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

func TestKind_Metadata(t *testing.T) {
	k := Kind{}
	assert.Equal(t, "preference_access", k.Name())
	assert.Equal(t, "prefacc-", k.IDPrefix())
	assert.True(t, k.Retention().AppendOnly)
	assert.Equal(t, memory.ComponentWritten, k.WriteAuthority())
}

func TestRecord_AppendsOneEntryPerCall(t *testing.T) {
	mem := memory.NewLocal(memoryinmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}
	ctx := memory.SystemContext(t.Context(), "test")

	c := Content{Ref: "email:alice@example.com", ResolvedSubject: "canon-abc", Outcome: OutcomeOK, Keys: []string{"language"}}
	require.NoError(t, Record(ctx, mem, scope, c))
	require.NoError(t, Record(ctx, mem, scope, c)) // append-only: a second identical read appends a second entry

	got, err := List(ctx, mem, scope)
	require.NoError(t, err)
	require.Len(t, got, 2, "append-only: every read is its own entry, even when byte-identical")
	assert.Equal(t, c.Ref, got[0].Ref)
	assert.Equal(t, c.ResolvedSubject, got[0].ResolvedSubject)
	assert.Equal(t, OutcomeOK, got[0].Outcome)
	assert.Equal(t, c.Keys, got[0].Keys)
}
