package userpreference_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/userpreference"
)

// newTestLocalFacade mirrors how the facade door tests in pkg/memory
// construct a Local: an inmem backend, no options — the door logic under
// test lives in the facade itself, not in any backend or logger behavior.
func newTestLocalFacade(t *testing.T) *memory.Local {
	t.Helper()
	return memory.NewLocal(inmem.NewBackend())
}

func TestEntryID_DeterministicAndPrefixed(t *testing.T) {
	a := userpreference.EntryID("ns", "reviewbot", "language")
	b := userpreference.EntryID("ns", "reviewbot", "language")
	assert.Equal(t, a, b)
	assert.True(t, strings.HasPrefix(a, userpreference.IDPrefix))
	// Field-boundary safety: ("ns","a","bc") != ("ns","ab","c").
	assert.NotEqual(t, userpreference.EntryID("ns", "a", "bc"), userpreference.EntryID("ns", "ab", "c"))
}

func TestKind_Registered(t *testing.T) {
	k, ok := memory.LookupKind(userpreference.KindName)
	require.True(t, ok)
	assert.Equal(t, userpreference.IDPrefix, k.IDPrefix())
	assert.Equal(t, memory.ComponentWritten, k.WriteAuthority())
	assert.Equal(t, []string{"classNamespace", "className"}, k.IndexedFields())
	assert.Equal(t, memory.Retention{SoftCapPerScope: userpreference.SoftCap}, k.Retention())
}

// The structural guard: a session bearer's Put of this ComponentWritten kind
// is refused at the facade's per-kind door before any logic runs.
func TestSessionBearerCannotWriteUserPreference(t *testing.T) {
	m := newTestLocalFacade(t)
	scope, err := memory.UserScope("YWxpY2U")
	require.NoError(t, err)
	ctx := memory.WithTokenSession(context.Background(), memory.NamespacedName{Namespace: "ns", Name: "sess"})
	ctx = memory.WithApproval(ctx, memory.ForBearerToken(memory.WriteMemory, scope.ID, "tok"))
	_, err = m.Put(ctx, memory.Entry{
		Scope: scope, Kind: userpreference.KindName,
		ID:      userpreference.EntryID("ns", "reviewbot", "language"),
		Content: json.RawMessage(`{"classNamespace":"ns","className":"reviewbot","key":"language","value":"\"de\""}`),
	})
	require.ErrorIs(t, err, memory.ErrKindNotSessionWritable)
}

func TestSystemWriteAndReread(t *testing.T) {
	m := newTestLocalFacade(t)
	scope, err := memory.UserScope("YWxpY2U")
	require.NoError(t, err)
	ctx := memory.SystemContext(context.Background(), "test")
	_, err = m.Put(ctx, memory.Entry{Scope: scope, Kind: userpreference.KindName,
		ID:      userpreference.EntryID("ns", "reviewbot", "language"),
		Content: json.RawMessage(`{"classNamespace":"ns","className":"reviewbot","key":"language","value":"\"de\""}`)})
	require.NoError(t, err)
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{userpreference.KindName}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
}
