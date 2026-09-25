package state_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// fakeKind is a minimal Kind impl used to drive registry behavior
// without depending on Plans (which is a separate package).
type fakeKind struct {
	name       string
	newStoreFn func(state.Deps) tool.StateStore
}

func (k *fakeKind) Name() string                             { return k.name }
func (k *fakeKind) NewStore(deps state.Deps) tool.StateStore { return k.newStoreFn(deps) }

type fakeStore struct {
	kindName string
	notes    []json.RawMessage
	err      error
}

func (s *fakeStore) Kind() string { return s.kindName }
func (s *fakeStore) ReplayNote(payload json.RawMessage) error {
	if s.err != nil {
		return s.err
	}
	s.notes = append(s.notes, append(json.RawMessage(nil), payload...))
	return nil
}

func TestRegister_AddsKind(t *testing.T) {
	t.Cleanup(state.ResetForTest())
	state.Register(&fakeKind{
		name:       "alpha",
		newStoreFn: func(state.Deps) tool.StateStore { return &fakeStore{kindName: "alpha"} },
	})
	r := state.NewRegistry(state.Deps{})
	got, ok := r.Get("alpha")
	require.True(t, ok)
	require.Equal(t, "alpha", got.Kind())
}

func TestRegister_PanicsOnDuplicate(t *testing.T) {
	t.Cleanup(state.ResetForTest())
	k := &fakeKind{name: "dup", newStoreFn: func(state.Deps) tool.StateStore { return &fakeStore{kindName: "dup"} }}
	state.Register(k)
	require.Panics(t, func() { state.Register(k) })
}

func TestNewRegistry_MaterializesAllKinds(t *testing.T) {
	t.Cleanup(state.ResetForTest())
	state.Register(&fakeKind{name: "a", newStoreFn: func(state.Deps) tool.StateStore { return &fakeStore{kindName: "a"} }})
	state.Register(&fakeKind{name: "b", newStoreFn: func(state.Deps) tool.StateStore { return &fakeStore{kindName: "b"} }})

	r := state.NewRegistry(state.Deps{})
	_, hasA := r.Get("a")
	_, hasB := r.Get("b")
	_, hasMissing := r.Get("nope")
	require.True(t, hasA)
	require.True(t, hasB)
	require.False(t, hasMissing)
}

func TestRegistry_Get_ReturnsFalseForUnknown(t *testing.T) {
	t.Cleanup(state.ResetForTest())
	r := state.NewRegistry(state.Deps{})
	_, ok := r.Get("missing")
	require.False(t, ok)
}

func TestDispatchSystemNote_RoutesByKind(t *testing.T) {
	t.Cleanup(state.ResetForTest())
	storeA := &fakeStore{kindName: "a"}
	storeB := &fakeStore{kindName: "b"}
	state.Register(&fakeKind{name: "a", newStoreFn: func(state.Deps) tool.StateStore { return storeA }})
	state.Register(&fakeKind{name: "b", newStoreFn: func(state.Deps) tool.StateStore { return storeB }})

	r := state.NewRegistry(state.Deps{})
	wrappedA := []byte(`{"kind":"a","v":1,"data":{"hello":"world"}}`)
	wrappedB := []byte(`{"kind":"b","v":1,"data":{"x":1}}`)
	matchedA, errA := state.DispatchSystemNote(r, wrappedA)
	require.True(t, matchedA)
	require.NoError(t, errA)
	matchedB, errB := state.DispatchSystemNote(r, wrappedB)
	require.True(t, matchedB)
	require.NoError(t, errB)

	require.Len(t, storeA.notes, 1)
	require.JSONEq(t, `{"hello":"world"}`, string(storeA.notes[0]))
	require.Len(t, storeB.notes, 1)
}

func TestDispatchSystemNote_LegacyNote_NotMatchedNoError(t *testing.T) {
	t.Cleanup(state.ResetForTest())
	r := state.NewRegistry(state.Deps{})
	// Legacy "delivered" note (no kind/data wrapper) — benign fall-through.
	matched, err := state.DispatchSystemNote(r, []byte(`{"delivered":["abc"]}`))
	require.False(t, matched)
	require.NoError(t, err)
}

func TestDispatchSystemNote_UnknownKind_NotMatchedNoError(t *testing.T) {
	t.Cleanup(state.ResetForTest())
	r := state.NewRegistry(state.Deps{})
	matched, err := state.DispatchSystemNote(r, []byte(`{"kind":"nope","v":1,"data":{}}`))
	require.False(t, matched)
	require.NoError(t, err)
}

func TestDispatchSystemNote_MalformedJSON_NotMatchedNoError(t *testing.T) {
	t.Cleanup(state.ResetForTest())
	r := state.NewRegistry(state.Deps{})
	matched, err := state.DispatchSystemNote(r, []byte(`{not json`))
	require.False(t, matched)
	require.NoError(t, err)
}

// A wrapped note for a registered Kind whose ReplayNote fails is the
// real-failure case the audit flagged: it MUST be reported as
// (matched=true, err!=nil), never collapsed into a benign fall-through,
// so the runner can log that the resumed session is missing that state
// rather than dropping the error silently.
func TestDispatchSystemNote_RegisteredKindReplayError_MatchedAndReturnsError(t *testing.T) {
	t.Cleanup(state.ResetForTest())
	storeErr := &fakeStore{kindName: "err", err: errors.New("boom")}
	state.Register(&fakeKind{name: "err", newStoreFn: func(state.Deps) tool.StateStore { return storeErr }})

	r := state.NewRegistry(state.Deps{})
	matched, err := state.DispatchSystemNote(r, []byte(`{"kind":"err","v":7,"data":{}}`))
	require.True(t, matched, "a wrapped note for a registered Kind matched even when ReplayNote fails")
	require.Error(t, err, "ReplayNote failure must not be silently dropped")
	require.ErrorContains(t, err, "boom", "the underlying ReplayNote error must be preserved (wrapped)")
	require.ErrorContains(t, err, `"err"`, "the error carries the kind for log context")
	require.ErrorContains(t, err, "v7", "the error carries the wrapper version for log context")
	require.ErrorIs(t, err, storeErr.err, "the underlying error is wrapped, not flattened")
}

func TestDeps_AppendSystemNote_Plumbed(t *testing.T) {
	t.Cleanup(state.ResetForTest())
	got := []map[string]any{}
	deps := state.Deps{
		AppendSystemNote: func(_ context.Context, content map[string]any) error {
			got = append(got, content)
			return nil
		},
	}
	state.Register(&fakeKind{
		name: "p",
		newStoreFn: func(d state.Deps) tool.StateStore {
			require.NotNil(t, d.AppendSystemNote)
			_ = d.AppendSystemNote(context.Background(), map[string]any{"hello": "world"})
			return &fakeStore{kindName: "p"}
		},
	})
	state.NewRegistry(deps)
	require.Len(t, got, 1)
	require.Equal(t, "world", got[0]["hello"])
}

// --- ResetForTest restore semantics -----------------------------------------

// initSentinelKind is registered from init() below, exactly the way the real
// Kinds in plans/, deliveries/ and triggerstatus/ register themselves. Nothing
// else in this package's test binary links a Kind package, so it stands in for
// everything init() installs — the thing a reset has to hand back.
const initSentinelKind = "init-sentinel"

func init() {
	state.Register(newFakeKind(initSentinelKind))
}

// newFakeKind builds a Kind whose Store just reports its own name — enough to
// tell "registered" from "not registered" through NewRegistry.
func newFakeKind(name string) state.Kind {
	return &fakeKind{
		name:       name,
		newStoreFn: func(state.Deps) tool.StateStore { return &fakeStore{kindName: name} },
	}
}

// isRegistered reports whether name is in the global Kind registry, by
// materializing a session Registry and asking it for that Kind's Store.
func isRegistered(t *testing.T, name string) bool {
	t.Helper()
	_, ok := state.NewRegistry(state.Deps{}).Get(name)
	return ok
}

func TestResetForTest_RestoreReinstatesInitRegisteredKinds(t *testing.T) {
	restore := state.ResetForTest()
	t.Cleanup(restore) // a require below aborting must not leak the cleared registry
	require.False(t, isRegistered(t, initSentinelKind),
		"the reset clears the registry for the duration of the caller's test")

	state.Register(newFakeKind("scratch"))
	require.True(t, isRegistered(t, "scratch"), "the caller can register against the cleared registry")

	restore()
	require.True(t, isRegistered(t, initSentinelKind),
		"restore reinstates the Kinds init() installed, so tests running afterwards still see them")
	require.False(t, isRegistered(t, "scratch"),
		"restore drops the Kinds the caller registered against the cleared registry")
}

func TestResetForTest_RestoreReplaysTheSnapshotOnEveryCall(t *testing.T) {
	restore := state.ResetForTest()
	t.Cleanup(restore)

	restore()
	require.True(t, isRegistered(t, initSentinelKind), "the first restore reinstates the snapshot")

	// Registering after a restore must not reach the snapshot the closure
	// holds, or a second restore would reinstate something the first never had.
	state.Register(newFakeKind("after-restore"))
	restore()
	require.True(t, isRegistered(t, initSentinelKind), "a repeated restore is safe, not destructive")
	require.False(t, isRegistered(t, "after-restore"),
		"each restore replays the snapshot, so it drops what was registered after the previous one")
}

func TestResetForTest_NestedResetsUnwindInOrder(t *testing.T) {
	outer := state.ResetForTest()
	t.Cleanup(outer)
	state.Register(newFakeKind("outer-kind"))

	inner := state.ResetForTest()
	require.False(t, isRegistered(t, "outer-kind"), "the inner reset clears what the outer scope registered")
	state.Register(newFakeKind("inner-kind"))

	inner()
	require.True(t, isRegistered(t, "outer-kind"), "the inner restore unwinds to the registry the outer reset left")
	require.False(t, isRegistered(t, "inner-kind"), "the inner restore drops the inner scope's own Kind")

	outer()
	require.True(t, isRegistered(t, initSentinelKind), "the outer restore unwinds to what init() installed")
	require.False(t, isRegistered(t, "outer-kind"), "the outer restore drops the outer scope's own Kind")
}

// This test must stay LAST in the file: Go runs a package's tests in source
// order, so it is the one that sees whatever every sibling reset above left
// behind. It is the in-situ form of the property — a reset that does not hand
// the registry back leaves every later test in the binary looking at an empty
// one, which passes alone and fails in company, and the only place that shows
// up is a test that runs after the resets.
func TestGlobalRegistry_SurvivesSiblingResets(t *testing.T) {
	require.True(t, isRegistered(t, initSentinelKind),
		"a Kind installed by init() is still registered after every sibling test in this package has reset the registry")
}
