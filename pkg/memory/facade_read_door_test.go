package memory_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// hiddenKind is a Kind a session credential may never read — the shape
// pt_tag_content has in production.
//
// A local fake rather than the real Kind: tests in this package call
// ResetRegistryForTest, so the globally-registered set is not dependable here,
// and the door is a property of the MECHANISM rather than of any one Kind.
type hiddenFakeKind struct{ name, prefix string }

func (k hiddenFakeKind) Name() string                                 { return k.name }
func (k hiddenFakeKind) IDPrefix() string                             { return k.prefix }
func (hiddenFakeKind) Retention() memory.Retention                    { return memory.Retention{} }
func (hiddenFakeKind) ContentSchema() reflect.Type                    { return nil }
func (hiddenFakeKind) IndexedFields() []string                        { return nil }
func (hiddenFakeKind) WriteAuthority() memory.WriteAuthority          { return memory.ComponentWritten }
func (hiddenFakeKind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return noopHooks{} }

// SessionReadable false is what puts this Kind behind the door.
func (hiddenFakeKind) SessionReadable() bool { return false }

const (
	readDoorScope = "nsA/sessA"
	hiddenKind    = "read-door-hidden"
	ordinaryKind  = "read-door-ordinary"
)

// sessionCtx is a caller holding a per-session credential — an agent's runner.
func sessionCtx() context.Context {
	ctx := memory.WithApproval(context.Background(),
		memory.ForBearerToken(memory.ReadMemory, readDoorScope, "tok-1"))
	return memory.WithTokenSession(ctx, memory.NamespacedName{Namespace: "nsA", Name: "sessA"})
}

// platformCtx is an in-process platform caller: no token session on the
// context, so the read door does not apply to it.
func platformCtx() context.Context {
	return memory.WithSystemApproval(context.Background(), "operator:test")
}

func newReadDoorStore(t *testing.T) *memory.Local {
	t.Helper()
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(hiddenFakeKind{name: hiddenKind, prefix: "rdh-"})
	memory.RegisterKind(fakeKind{name: ordinaryKind, prefix: "rdo-"})

	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: readDoorScope}
	for _, e := range []memory.Entry{
		{Scope: scope, Kind: hiddenKind, ID: "rdh-1", CreatedAt: time.Unix(0, 0).UTC(), Content: json.RawMessage(`{}`)},
		{Scope: scope, Kind: ordinaryKind, ID: "rdo-1", CreatedAt: time.Unix(0, 0).UTC(), Content: json.RawMessage(`{}`)},
	} {
		_, err := m.Put(platformCtx(), e)
		require.NoError(t, err, "seeding %s", e.Kind)
	}
	return m
}

// TestReadDoorRefusesASessionNamingAHiddenKind is the LOUD half.
//
// An explicit ask for a platform-only Kind is a bug or an attempt. Answering
// it with an empty result would read as "there is nothing there", and the
// caller would carry on believing it had looked.
func TestReadDoorRefusesASessionNamingAHiddenKind(t *testing.T) {
	m := newReadDoorStore(t)

	_, err := m.Query(sessionCtx(), memory.Query{
		Scope: memory.Scope{Kind: "session", ID: readDoorScope},
		Kinds: []string{hiddenKind},
	})
	require.ErrorIs(t, err, memory.ErrKindNotSessionReadable,
		"a session asking for platform-only content by name must be refused, not answered empty")
}

// TestReadDoorFiltersAHiddenKindFromABroadSweep is the SILENT half.
//
// A query narrowing on nothing legitimately sweeps the scope, and erroring
// would make every broad query fail the moment one platform-only record
// existed. The caller asked for "everything I may see", and this is what that
// means — but the hidden entry must not be in it.
func TestReadDoorFiltersAHiddenKindFromABroadSweep(t *testing.T) {
	m := newReadDoorStore(t)

	res, err := m.Query(sessionCtx(), memory.Query{
		Scope: memory.Scope{Kind: "session", ID: readDoorScope},
	})
	require.NoError(t, err, "a broad sweep stays legal; it is simply narrower than the store")

	var kinds []string
	for _, e := range res.Entries {
		kinds = append(kinds, e.Kind)
	}
	assert.NotContains(t, kinds, hiddenKind,
		"hiding a Kind is a formality if it comes back through an unnarrowed query")
	assert.Contains(t, kinds, ordinaryKind,
		"the door must remove the hidden Kind and nothing else")
}

// TestReadDoorDoesNotApplyToPlatformCallers pins that the door restricts the
// SESSION, not the platform. The platform is precisely who holds this content
// on the session's behalf; a door that blocked it too would make the content
// unresolvable by anyone and the store pointless.
func TestReadDoorDoesNotApplyToPlatformCallers(t *testing.T) {
	m := newReadDoorStore(t)

	res, err := m.Query(platformCtx(), memory.Query{
		Scope: memory.Scope{Kind: "session", ID: readDoorScope},
		Kinds: []string{hiddenKind},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1, "the platform must still resolve what it stored")
	assert.Equal(t, "rdh-1", res.Entries[0].ID)
}

// TestReadDoorLeavesOrdinaryKindsAlone stops the door from becoming a blanket
// restriction. If this fails, every agent's memory tools went dark at once.
func TestReadDoorLeavesOrdinaryKindsAlone(t *testing.T) {
	m := newReadDoorStore(t)

	res, err := m.Query(sessionCtx(), memory.Query{
		Scope: memory.Scope{Kind: "session", ID: readDoorScope},
		Kinds: []string{ordinaryKind},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "rdo-1", res.Entries[0].ID)
}
