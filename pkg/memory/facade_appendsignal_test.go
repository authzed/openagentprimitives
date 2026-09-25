package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

func TestPut_appendOnlyEmitsEntryAppendedSignal(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	var got []memory.Signal

	m := newTestFacadeWithHook(t, func(_ context.Context, sig memory.Signal) error {
		got = append(got, sig)
		return nil
	})

	_, err := m.Put(ctx, newSignedAppendOnlyEntry(t, "scope-1"))
	require.NoError(t, err)

	require.Len(t, got, 1, "a successful append-only Put must fan a signal out to ScopeHooks")
	assert.Equal(t, memory.SignalEntryAppended, got[0].Kind)
	assert.Equal(t, "scope-1", got[0].Scope.ID)
}

// TestPut_appendOnlySignalFiresWithoutCallerApproval proves the wrap at
// facade.go's signal dispatch (WithSystemApproval before SendSignal) is what
// lets the entry-appended signal fire, independent of whatever approval the
// caller happened to present. The outer ctx here is bare context.Background()
// — no system approval, no WriteMemory approval of any kind — the minimal
// context a real in-process caller could present: Put's own append-only path
// requires no approval on entry (authorization there is provenance
// verification, not a capability door; see TestLocal_ProvenanceVerifierEnforcedOnAppendOnly
// in facade_test.go, which already exercises Put on a bare context), so this
// isolates the wrap under test without smuggling in an approval the test
// itself supplied. Without the wrap, ctx reaching SendSignal would carry only
// the internal-tier AppendAudit approval Put mints for its own append-only
// door — which does not satisfy SendSignal's WriteMemory door — so the signal
// would silently fail to dispatch and this test would fail on the Len assertion.
func TestPut_appendOnlySignalFiresWithoutCallerApproval(t *testing.T) {
	ctx := context.Background()
	var got []memory.Signal

	m := newTestFacadeWithHook(t, func(_ context.Context, sig memory.Signal) error {
		got = append(got, sig)
		return nil
	})

	_, err := m.Put(ctx, newSignedAppendOnlyEntry(t, "scope-1"))
	require.NoError(t, err, "append-only Put needs no caller-supplied approval; authorization is provenance verification")

	require.Len(t, got, 1, "the signal must fire even though the caller's context carried no approval of its own")
	assert.Equal(t, memory.SignalEntryAppended, got[0].Kind)
	assert.Equal(t, "scope-1", got[0].Scope.ID)
}

func TestPut_nonAppendOnlyEmitsNoSignal(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	var got []memory.Signal

	m := newTestFacadeWithHook(t, func(_ context.Context, sig memory.Signal) error {
		got = append(got, sig)
		return nil
	})

	_, err := m.Put(ctx, newPlainEntry(t, "scope-1"))
	require.NoError(t, err)
	assert.Empty(t, got, "only append-only kinds signal on write")
}

func TestPut_appendOnlyIdempotentRePutEmitsNoAdditionalSignal(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	var got []memory.Signal

	m := newTestFacadeWithHook(t, func(_ context.Context, sig memory.Signal) error {
		got = append(got, sig)
		return nil
	})

	e := newSignedAppendOnlyEntry(t, "scope-1")
	_, err := m.Put(ctx, e)
	require.NoError(t, err, "first put creates")
	require.Len(t, got, 1, "first put signals once")

	_, err = m.Put(ctx, e)
	require.NoError(t, err, "byte-identical re-put is idempotent no-op")
	assert.Len(t, got, 1, "an idempotent re-put of an already-landed entry must not emit a second signal")
}

func TestPut_appendOnlySignalHookErrorDoesNotFailPut(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	m := newTestFacadeWithHook(t, func(_ context.Context, _ memory.Signal) error {
		return errors.New("hook boom")
	})

	_, err := m.Put(ctx, newSignedAppendOnlyEntry(t, "scope-1"))
	assert.NoError(t, err, "a hook error must not fail a write that already landed durably")
}

// TestPut_appendOnlySignalHookRecursion_FiresExactlyOnce is the regression
// test for the append-only signal fan-out recursing without bound: a
// ScopeHooks that performs an append-only Put in reaction to
// SignalEntryAppended, recording the signal as a new entry with a freshly
// randomized ID each time, must fire exactly once per caller-initiated Put,
// never recursing on the entries its own reaction writes. Guards the general
// hazard for ANY such hook — pkg/memory/kinds/lifecycle, the hook that
// exposed this in production, instead opts out of reacting to this specific
// signal entirely (see its OnSignal), so this synthetic hook is what now
// exercises the facade's guard.
//
// Each recursive write mints a fresh ID, so the append-only idempotent-re-put
// short-circuit in Put never applies and cannot be relied on to break the
// cycle. The hook here caps itself at maxRecursiveCalls rather than recursing
// without bound, so an unguarded facade fails this test fast — on the wrong
// call count — instead of hanging or crashing the test binary with a stack
// overflow.
func TestPut_appendOnlySignalHookRecursion_FiresExactlyOnce(t *testing.T) {
	const maxRecursiveCalls = 100

	ctx := memory.WithSystemApproval(context.Background(), "test")
	var m *memory.Local
	calls := 0
	nextID := 0

	hook := func(hookCtx context.Context, sig memory.Signal) error {
		calls++
		if calls > maxRecursiveCalls {
			return fmt.Errorf("recursion guard tripped: OnSignal invoked %d times", calls)
		}
		nextID++
		_, err := m.Put(hookCtx, memory.Entry{
			Scope:     sig.Scope,
			Kind:      "sig-appendonly",
			ID:        fmt.Sprintf("sigao-recur-%d", nextID),
			CreatedAt: time.Unix(0, 0).UTC(),
			Content:   json.RawMessage(`{"v":1}`),
		})
		return err
	}

	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendSignalTestKind{hooks: appendSignalTestHooks{fn: hook}})
	m = memory.NewLocal(inmem.NewBackend(), memory.WithProvenanceVerifier(&fakeVerifier{}))

	_, err := m.Put(ctx, newSignedAppendOnlyEntry(t, "scope-1"))
	require.NoError(t, err, "a hook error must not fail the write that already landed")

	assert.Equal(t, 1, calls,
		"a hook that appends in reaction to SignalEntryAppended must fire exactly once, never recursing on its own write")
}

// appendSignalTestHooks forwards every OnSignal call it receives to fn — the
// vehicle newTestFacadeWithHook uses to observe signals dispatched to a
// registered Kind's ScopeHooks.
type appendSignalTestHooks struct {
	fn func(context.Context, memory.Signal) error
}

func (h appendSignalTestHooks) OnSignal(ctx context.Context, sig memory.Signal) error {
	return h.fn(ctx, sig)
}

// appendSignalTestKind is a test-local append-only Kind whose ScopeHooks is
// the caller-supplied hooks value, so a test can observe every signal
// dispatched to it.
type appendSignalTestKind struct {
	hooks memory.ScopeHooks
}

func (appendSignalTestKind) Name() string     { return "sig-appendonly" }
func (appendSignalTestKind) IDPrefix() string { return "sigao-" }
func (appendSignalTestKind) Retention() memory.Retention {
	return memory.Retention{AppendOnly: true}
}
func (appendSignalTestKind) ContentSchema() reflect.Type { return nil }
func (appendSignalTestKind) IndexedFields() []string     { return nil }

// WriteAuthority: a stand-in for an ordinary agent-authored append-only kind;
// the per-kind write door is exercised against the real registry in
// pkg/memory/httpsrv and pkg/memory/kinds/all.
func (appendSignalTestKind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (k appendSignalTestKind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks {
	return k.hooks
}

// newTestFacadeWithHook registers an append-only test Kind whose ScopeHooks
// forwards every signal it receives to hook, alongside a plain (mutable)
// test Kind that registers no hook at all, and returns a facade wired with a
// ProvenanceVerifier that accepts every entry — the same fakeVerifier
// fixture facade_test.go uses to let an append-only Put succeed without
// exercising real cryptographic signing.
func newTestFacadeWithHook(t *testing.T, hook func(context.Context, memory.Signal) error) *memory.Local {
	t.Helper()
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendSignalTestKind{hooks: appendSignalTestHooks{fn: hook}})
	memory.RegisterKind(fakeKind{name: "sig-plain", prefix: "sigp-"})
	return memory.NewLocal(inmem.NewBackend(), memory.WithProvenanceVerifier(&fakeVerifier{}))
}

// newSignedAppendOnlyEntry returns an Entry of the append-only test Kind.
// "Signed" here means it is verified by the facade's configured
// ProvenanceVerifier (fakeVerifier, wired in newTestFacadeWithHook) rather
// than by a real cryptographic signature.
func newSignedAppendOnlyEntry(t *testing.T, scopeID string) memory.Entry {
	t.Helper()
	return memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: scopeID},
		Kind:      "sig-appendonly",
		ID:        "sigao-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	}
}

// newPlainEntry returns an Entry of the plain (mutable, non-append-only)
// test Kind registered by newTestFacadeWithHook.
func newPlainEntry(t *testing.T, scopeID string) memory.Entry {
	t.Helper()
	return memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: scopeID},
		Kind:      "sig-plain",
		ID:        "sigp-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	}
}
