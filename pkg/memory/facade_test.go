package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// appendOnlyKind is a test-local Kind whose Retention marks it
// append-only, exercising the facade's append-only enforcement without
// depending on a specific production Kind.
type appendOnlyKind struct {
	name, prefix string
}

func (k appendOnlyKind) Name() string     { return k.name }
func (k appendOnlyKind) IDPrefix() string { return k.prefix }
func (appendOnlyKind) Retention() memory.Retention {
	return memory.Retention{AppendOnly: true}
}
func (appendOnlyKind) ContentSchema() reflect.Type { return nil }
func (appendOnlyKind) IndexedFields() []string     { return nil }

// WriteAuthority: a stand-in for an ordinary agent-authored kind; the
// per-kind write door is exercised against the real registry in
// pkg/memory/httpsrv and pkg/memory/kinds/all.
func (appendOnlyKind) WriteAuthority() memory.WriteAuthority          { return memory.SessionWritten }
func (appendOnlyKind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return noopHooks{} }

func TestPutAppendOnlyConflict(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendOnlyKind{name: "ao", prefix: "ao-"})
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := context.Background()

	e := memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:      "ao",
		ID:        "ao-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	}
	_, err := mem.Put(ctx, e)
	require.NoError(t, err, "first put creates")

	_, err = mem.Put(ctx, e)
	assert.NoError(t, err, "byte-identical re-put is idempotent no-op")

	e2 := e
	e2.Content = json.RawMessage(`{"changed":true}`)
	_, err = mem.Put(ctx, e2)
	assert.ErrorIs(t, err, memory.ErrAppendOnlyConflict)
}

func TestDeleteAppendOnlyKindRefused(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendOnlyKind{name: "ao", prefix: "ao-"})
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")

	e := memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:      "ao",
		ID:        "ao-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	}
	_, err := mem.Put(ctx, e)
	require.NoError(t, err, "put append-only entry")

	err = mem.Delete(ctx, e.Scope, e.Kind, e.ID)
	assert.ErrorIs(t, err, memory.ErrAppendOnlyKind)

	// Scope deletion is STRUCTURALLY unable to remove an append-only entry — the
	// former retention escape hatch is closed, so the tamper-evident audit is
	// permanent and survives a session's teardown.
	require.NoError(t, mem.DeleteScope(ctx, e.Scope), "DeleteScope succeeds")
	_, ok, gerr := mem.Get(ctx, e.Scope, e.Kind, e.ID)
	require.NoError(t, gerr)
	assert.True(t, ok, "append-only entry MUST survive DeleteScope (no code path may erase the audit)")
}

func TestPutMutableKindStillUpserts(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(fakeKind{name: "mutable", prefix: "m-"})
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")

	e := memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:      "mutable",
		ID:        "m-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	}
	_, err := mem.Put(ctx, e)
	require.NoError(t, err, "first put")

	e2 := e
	e2.Content = json.RawMessage(`{"v":2}`)
	_, err = mem.Put(ctx, e2)
	assert.NoError(t, err, "mutable kind re-put with different content is allowed")
}

// --- ProvenanceVerifier integration tests ---

// fakeVerifier records its call count and returns a configurable error.
type fakeVerifier struct {
	mu      sync.Mutex
	calls   int
	returns error
}

func (v *fakeVerifier) VerifyEntry(_ context.Context, _ string, _ memory.Entry) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	return v.returns
}

func TestLocal_ProvenanceVerifierEnforcedOnAppendOnly(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendOnlyKind{name: "ao", prefix: "ao-"})
	b := &fakeBackend{}
	v := &fakeVerifier{} // returns nil → verify succeeds
	m := memory.NewLocal(b, memory.WithProvenanceVerifier(v))

	_, err := m.Put(context.Background(), memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:      "ao",
		ID:        "ao-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	})
	require.NoError(t, err, "append-only Put with passing verifier succeeds")
	assert.Equal(t, 1, v.calls, "verifier called once for the new append-only entry")
	assert.Len(t, b.puts, 1, "backend.Put runs after verify passes")
}

func TestLocal_ProvenanceVerifierRejectsAppendOnly(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendOnlyKind{name: "ao", prefix: "ao-"})
	b := &fakeBackend{}
	v := &fakeVerifier{returns: memory.ErrProvenanceRequired}
	m := memory.NewLocal(b, memory.WithProvenanceVerifier(v))

	_, err := m.Put(context.Background(), memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:      "ao",
		ID:        "ao-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	})
	require.ErrorIs(t, err, memory.ErrProvenanceRequired, "verify failure propagates verbatim")
	assert.Equal(t, 1, v.calls)
	assert.Empty(t, b.puts, "backend.Put must not run when verify rejects")
}

func TestProvenanceVerifierNotCalledOnRePutOrConflict(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendOnlyKind{name: "ao", prefix: "ao-"})

	v := &fakeVerifier{} // returns nil → verify succeeds
	m := memory.NewLocal(inmem.NewBackend(), memory.WithProvenanceVerifier(v))
	ctx := context.Background()

	e := memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:      "ao",
		ID:        "ao-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	}

	// First put: new entry → verifier called once.
	_, err := m.Put(ctx, e)
	require.NoError(t, err, "first put must succeed")
	assert.Equal(t, 1, v.calls, "verifier called once for the new entry")

	// Byte-identical re-put: idempotent no-op → verifier NOT called again.
	_, err = m.Put(ctx, e)
	assert.NoError(t, err, "byte-identical re-put is idempotent no-op")
	assert.Equal(t, 1, v.calls, "verifier must NOT be called on an idempotent re-put")

	// Conflicting re-put: different content → ErrAppendOnlyConflict, verifier NOT called.
	e2 := e
	e2.Content = json.RawMessage(`{"changed":true}`)
	_, err = m.Put(ctx, e2)
	assert.ErrorIs(t, err, memory.ErrAppendOnlyConflict, "conflicting re-put must return ErrAppendOnlyConflict")
	assert.Equal(t, 1, v.calls, "verifier must NOT be called when an append-only conflict is detected")
}

func TestLocal_ProvenanceVerifierNotCalledOnMutableKind(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(fakeKind{name: "mutable", prefix: "m-"})
	b := &fakeBackend{}
	v := &fakeVerifier{returns: memory.ErrProvenanceRequired} // would reject if called
	m := memory.NewLocal(b, memory.WithProvenanceVerifier(v))

	ctx := memory.WithSystemApproval(context.Background(), "test")
	_, err := m.Put(ctx, memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:      "mutable",
		ID:        "m-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	})
	require.NoError(t, err, "mutable-kind Put bypasses verify entirely")
	assert.Equal(t, 0, v.calls, "verifier NOT called for a mutable kind")
	assert.Len(t, b.puts, 1)
}

// fakeBackend is a minimal Backend for facade tests. inmem comes later.
type fakeBackend struct {
	mu     sync.Mutex
	puts   []memory.Entry
	getErr error
}

func (b *fakeBackend) Capabilities() memory.Capabilities {
	return memory.Capabilities{LinkTraversal: 1, ReverseLinks: true, TagFilters: true, TimeRange: true}
}

func (b *fakeBackend) Put(_ context.Context, e memory.Entry) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.puts = append(b.puts, e)
	return nil
}

func (b *fakeBackend) Get(_ context.Context, _ memory.Scope, _, _ string) (memory.Entry, bool, error) {
	return memory.Entry{}, false, b.getErr
}

func (b *fakeBackend) Delete(_ context.Context, _ memory.Scope, _, _ string) error { return nil }

func (b *fakeBackend) DeleteScope(_ context.Context, _ memory.Scope) error { return nil }

func (b *fakeBackend) Query(_ context.Context, _ memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, nil
}

func (b *fakeBackend) QueryAllScopes(_ context.Context, _ memory.CrossScopeQuery) (memory.QueryResult, error) {
	return memory.QueryResult{}, nil
}

func (b *fakeBackend) Status(_ context.Context, _ memory.Scope) (memory.ScopeStatus, error) {
	return memory.ScopeStatus{State: memory.StatusUnknown}, nil
}

func TestMemoryPut_UnknownKindRejected(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	b := &fakeBackend{}
	m := memory.NewLocal(b)
	_, err := m.Put(context.Background(), memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:  "unknown_kind",
		ID:    "x-1",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown Kind")
	assert.Empty(t, b.puts, "backend.Put should not be called")
}

func TestMemoryPut_IDPrefixEnforced(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(fakeKind{name: "alpha", prefix: "a-"})
	b := &fakeBackend{}
	m := memory.NewLocal(b)

	_, err := m.Put(context.Background(), memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:  "alpha",
		ID:    "wrong-prefix-x",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prefix")
	assert.Empty(t, b.puts)
}

func TestMemoryPut_LinkPrefixValidated(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(fakeKind{name: "alpha", prefix: "a-"})
	memory.RegisterKind(fakeKind{name: "beta", prefix: "b-"})
	b := &fakeBackend{}
	m := memory.NewLocal(b)

	_, err := m.Put(context.Background(), memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:  "alpha",
		ID:    "a-1",
		Links: []memory.Link{{Relation: "rel", Kind: "beta", ID: "wrong-prefix-x"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Links[0]")
	assert.Empty(t, b.puts)
}

func TestMemoryPut_UnknownLinkKindSkipsValidation(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(fakeKind{name: "alpha", prefix: "a-"})
	b := &fakeBackend{}
	m := memory.NewLocal(b)

	ctx := memory.WithSystemApproval(context.Background(), "test")
	_, err := m.Put(ctx, memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:  "alpha",
		ID:    "a-1",
		Links: []memory.Link{{Relation: "for_turn", Kind: "turn", ID: "anything"}},
	})
	require.NoError(t, err)
	assert.Len(t, b.puts, 1)
}

func TestMemoryPut_HappyPath(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(fakeKind{name: "alpha", prefix: "a-"})
	b := &fakeBackend{}
	m := memory.NewLocal(b)

	e := memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:      "alpha",
		ID:        "a-1",
		CreatedAt: time.Now().UTC(),
	}
	ctx := memory.WithSystemApproval(context.Background(), "test")
	stored, err := m.Put(ctx, e)
	require.NoError(t, err)
	assert.Equal(t, e, stored, "Put returns the stored Entry")
	require.Len(t, b.puts, 1)
	assert.Equal(t, e, b.puts[0])
}

func TestMemoryGet_DelegatesToBackend(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	b := &fakeBackend{getErr: errors.New("boom")}
	m := memory.NewLocal(b)
	_, _, err := m.Get(context.Background(),
		memory.Scope{Kind: "session", ID: "ns/a"}, "k", "id")
	require.Error(t, err)
	assert.EqualError(t, err, "boom")
}

func TestMemoryQuery_DelegatesToBackend(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	b := &fakeBackend{}
	m := memory.NewLocal(b)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	res, err := m.Query(ctx, memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
	})
	require.NoError(t, err)
	assert.Empty(t, res.Entries)
	assert.False(t, res.Partial)
}

func TestMemoryStatus_DelegatesToBackend(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	b := &fakeBackend{}
	m := memory.NewLocal(b)
	st, err := m.Status(context.Background(), memory.Scope{Kind: "session", ID: "ns/a"})
	require.NoError(t, err)
	assert.Equal(t, memory.StatusUnknown, st.State)
}

// fakeHooks records every signal it sees.
type fakeHooks struct {
	mu      sync.Mutex
	got     []memory.Signal
	returns error
}

func (h *fakeHooks) OnSignal(_ context.Context, sig memory.Signal) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, sig)
	return h.returns
}

// kindWithHooks returns a Kind whose NewScopeHooks always returns h.
func kindWithHooks(name, prefix string, h memory.ScopeHooks) memory.Kind {
	return hookKind{name: name, prefix: prefix, hooks: h}
}

type hookKind struct {
	name, prefix string
	hooks        memory.ScopeHooks
}

func (k hookKind) Name() string              { return k.name }
func (k hookKind) IDPrefix() string          { return k.prefix }
func (hookKind) Retention() memory.Retention { return memory.Retention{} }
func (hookKind) ContentSchema() reflect.Type { return nil }
func (hookKind) IndexedFields() []string     { return nil }

// WriteAuthority: a stand-in for an ordinary agent-authored kind; the
// per-kind write door is exercised against the real registry in
// pkg/memory/httpsrv and pkg/memory/kinds/all.
func (k hookKind) WriteAuthority() memory.WriteAuthority          { return memory.SessionWritten }
func (k hookKind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return k.hooks }

func TestSendSignal_FanOutToAllRegisteredKinds(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	a := &fakeHooks{}
	b := &fakeHooks{}
	memory.RegisterKind(kindWithHooks("alpha", "a-", a))
	memory.RegisterKind(kindWithHooks("beta", "b-", b))

	m := memory.NewLocal(&fakeBackend{})
	sig := memory.Signal{
		Kind:  memory.SignalKind("test/ping"),
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		At:    time.Now().UTC(),
	}
	require.NoError(t, m.SendSignal(memory.WithSystemApproval(context.Background(), "test"), sig))

	assert.Equal(t, []memory.Signal{sig}, a.got)
	assert.Equal(t, []memory.Signal{sig}, b.got)
}

func TestSendSignal_LazyHookMaterialization(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	var newScopeHooksCalls int32
	memory.RegisterKind(countingKind{name: "alpha", prefix: "a-", calls: &newScopeHooksCalls})

	m := memory.NewLocal(&fakeBackend{})
	sig1 := memory.Signal{Kind: "test/ping", Scope: memory.Scope{Kind: "session", ID: "ns/a"}, At: time.Now().UTC()}
	sig2 := memory.Signal{Kind: "test/ping", Scope: memory.Scope{Kind: "session", ID: "ns/a"}, At: time.Now().UTC()}
	sig3 := memory.Signal{Kind: "test/ping", Scope: memory.Scope{Kind: "session", ID: "ns/b"}, At: time.Now().UTC()}

	ctx := memory.WithSystemApproval(context.Background(), "test")
	require.NoError(t, m.SendSignal(ctx, sig1))
	require.NoError(t, m.SendSignal(ctx, sig2)) // same scope — should reuse
	require.NoError(t, m.SendSignal(ctx, sig3)) // different scope — should materialize

	// Materialized once per (Scope, Kind): two distinct scopes → two calls.
	assert.EqualValues(t, 2, newScopeHooksCalls)
}

type countingKind struct {
	name, prefix string
	calls        *int32
}

func (k countingKind) Name() string              { return k.name }
func (k countingKind) IDPrefix() string          { return k.prefix }
func (countingKind) Retention() memory.Retention { return memory.Retention{} }
func (countingKind) ContentSchema() reflect.Type { return nil }
func (countingKind) IndexedFields() []string     { return nil }

// WriteAuthority: a stand-in for an ordinary agent-authored kind; the
// per-kind write door is exercised against the real registry in
// pkg/memory/httpsrv and pkg/memory/kinds/all.
func (k countingKind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (k countingKind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks {
	atomic.AddInt32(k.calls, 1)
	return &fakeHooks{}
}

func TestSendSignal_HookErrorsCollectedNotAborted(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	a := &fakeHooks{returns: errors.New("alpha bad")}
	b := &fakeHooks{} // succeeds
	memory.RegisterKind(kindWithHooks("alpha", "a-", a))
	memory.RegisterKind(kindWithHooks("beta", "b-", b))

	m := memory.NewLocal(&fakeBackend{})
	sig := memory.Signal{Kind: "test/ping", Scope: memory.Scope{Kind: "session", ID: "ns/a"}, At: time.Now().UTC()}
	err := m.SendSignal(memory.WithSystemApproval(context.Background(), "test"), sig)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alpha bad")
	// Beta still got the signal — fan-out wasn't aborted by alpha's error.
	assert.Equal(t, []memory.Signal{sig}, b.got)
}

// --- Authorizer integration tests ---

type fakeAuthorizer struct {
	mu           sync.Mutex
	putCalls     []memory.Entry
	queryCalls   int
	deleteCalls  []string
	cleanupCalls []memory.Scope
	denyPut      bool
	denyDelete   bool
	filterQuery  func([]memory.Entry) []memory.Entry
}

func (a *fakeAuthorizer) AuthorizePut(_ context.Context, e memory.Entry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.putCalls = append(a.putCalls, e)
	if a.denyPut {
		return errors.New("put denied")
	}
	return nil
}

func (a *fakeAuthorizer) AuthorizeQuery(_ context.Context, entries []memory.Entry) ([]memory.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queryCalls++
	if a.filterQuery != nil {
		return a.filterQuery(entries), nil
	}
	return entries, nil
}

func (a *fakeAuthorizer) AuthorizeDelete(_ context.Context, _ memory.Scope, _, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deleteCalls = append(a.deleteCalls, id)
	if a.denyDelete {
		return errors.New("delete denied")
	}
	return nil
}

func (a *fakeAuthorizer) CleanupScope(_ context.Context, scope memory.Scope) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleanupCalls = append(a.cleanupCalls, scope)
	return nil
}

type queryableBackend struct {
	fakeBackend
	entries []memory.Entry
}

func (b *queryableBackend) Query(_ context.Context, _ memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{Entries: b.entries}, nil
}

func TestNewLocal_NilAuthorizerBackwardsCompat(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(fakeKind{name: "alpha", prefix: "a-"})
	b := &fakeBackend{}
	m := memory.NewLocal(b) // no WithAuthorizer

	ctx := memory.WithSystemApproval(context.Background(), "test")
	_, err := m.Put(ctx, memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:  "alpha",
		ID:    "a-1",
	})
	require.NoError(t, err)
	assert.Len(t, b.puts, 1, "Put should succeed with nil authorizer")
}

func TestLocal_AuthorizerDeniesPut(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(fakeKind{name: "alpha", prefix: "a-"})
	b := &fakeBackend{}
	az := &fakeAuthorizer{denyPut: true}
	m := memory.NewLocal(b, memory.WithAuthorizer(az))

	ctx := memory.WithCaller(context.Background(), "user-1")
	ctx = memory.WithSystemApproval(ctx, "test")
	_, err := m.Put(ctx, memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:  "alpha",
		ID:    "a-1",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "put denied")
	assert.Empty(t, b.puts, "backend.Put should not be called when authz denies")
}

func TestLocal_AuthorizerFiltersQuery(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	b := &queryableBackend{
		entries: []memory.Entry{
			{Scope: memory.Scope{Kind: "session", ID: "ns/a"}, Kind: "alpha", ID: "a-1"},
			{Scope: memory.Scope{Kind: "session", ID: "ns/a"}, Kind: "alpha", ID: "a-2"},
			{Scope: memory.Scope{Kind: "session", ID: "ns/a"}, Kind: "alpha", ID: "a-3"},
		},
	}
	az := &fakeAuthorizer{
		filterQuery: func(entries []memory.Entry) []memory.Entry {
			var out []memory.Entry
			for _, e := range entries {
				if e.ID == "a-1" || e.ID == "a-3" {
					out = append(out, e)
				}
			}
			return out
		},
	}
	m := memory.NewLocal(b, memory.WithAuthorizer(az))

	ctx := memory.WithCaller(context.Background(), "user-1")
	ctx = memory.WithSystemApproval(ctx, "test")
	res, err := m.Query(ctx, memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
	})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 2)
	assert.Equal(t, "a-1", res.Entries[0].ID)
	assert.Equal(t, "a-3", res.Entries[1].ID)
}

func TestLocal_AuthorizerDeniesDelete(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	b := &fakeBackend{}
	az := &fakeAuthorizer{denyDelete: true}
	m := memory.NewLocal(b, memory.WithAuthorizer(az))

	ctx := memory.WithCaller(context.Background(), "user-1")
	ctx = memory.WithSystemApproval(ctx, "test")
	err := m.Delete(ctx, memory.Scope{Kind: "session", ID: "ns/a"}, "alpha", "a-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete denied")
}

func TestLocal_DeleteScopeDoesNotCleanupAuthz(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	b := &fakeBackend{}
	az := &fakeAuthorizer{}
	m := memory.NewLocal(b, memory.WithAuthorizer(az))

	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	err := m.DeleteScope(memory.WithSystemApproval(context.Background(), "test"), scope)
	require.NoError(t, err)
	// DeleteScope must NOT clean up the scope's authz grants: the retained
	// append-only audit stays queryable after the session is gone only if its
	// scope read-grants survive. Wiping them would orphan the permanent audit.
	assert.Empty(t, az.cleanupCalls, "DeleteScope must not remove the scope's authz grants (retained audit must stay readable)")
}

// --- Search provider integration tests ---

type fakeSearchProvider struct {
	name      string
	indexed   []string
	deleted   []string
	scopesDel []memory.Scope
	mu        sync.Mutex
}

func (p *fakeSearchProvider) Name() string { return p.name }
func (p *fakeSearchProvider) SearchCapabilities() memory.SearchCapabilities {
	return memory.SearchCapabilities{}
}
func (p *fakeSearchProvider) Search(_ context.Context, _ memory.SearchRequest) (memory.SearchResult, error) {
	return memory.SearchResult{}, nil
}
func (p *fakeSearchProvider) Index(_ context.Context, _ memory.Scope, _, id string, _ []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.indexed = append(p.indexed, id)
	return nil
}
func (p *fakeSearchProvider) DeleteIndex(_ context.Context, _ memory.Scope, _, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted = append(p.deleted, id)
	return nil
}
func (p *fakeSearchProvider) DeleteScopeIndex(_ context.Context, scope memory.Scope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scopesDel = append(p.scopesDel, scope)
	return nil
}

func TestLocal_PutIndexesSearchProviders(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(fakeKind{name: "alpha", prefix: "a-"})
	b := &fakeBackend{}
	sp := &fakeSearchProvider{name: "test"}
	m := memory.NewLocal(b, memory.WithSearchProviders(sp))

	ctx := memory.WithSystemApproval(context.Background(), "test")
	_, err := m.Put(ctx, memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:  "alpha", ID: "a-1",
		Content: []byte(`{"x":1}`),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a-1"}, sp.indexed)
}

func TestLocal_DeleteCallsDeleteIndex(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	b := &fakeBackend{}
	sp := &fakeSearchProvider{name: "test"}
	m := memory.NewLocal(b, memory.WithSearchProviders(sp))

	err := m.Delete(memory.WithSystemApproval(context.Background(), "test"), memory.Scope{Kind: "session", ID: "ns/a"}, "alpha", "a-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"a-1"}, sp.deleted)
}

func TestLocal_DeleteScopeRemovesOnlyEphemeralIndexes(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(fakeKind{name: "mutable", prefix: "m-"})
	memory.RegisterKind(appendOnlyKind{name: "ao", prefix: "ao-"})
	sp := &fakeSearchProvider{name: "test"}
	m := memory.NewLocal(inmem.NewBackend(), memory.WithSearchProviders(sp))
	ctx := memory.WithSystemApproval(context.Background(), "test")

	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	_, err := m.Put(ctx, memory.Entry{Scope: scope, Kind: "mutable", ID: "m-1", CreatedAt: time.Unix(0, 0).UTC(), Content: json.RawMessage(`{}`)})
	require.NoError(t, err)
	_, err = m.Put(ctx, memory.Entry{Scope: scope, Kind: "ao", ID: "ao-1", CreatedAt: time.Unix(0, 0).UTC(), Content: json.RawMessage(`{}`)})
	require.NoError(t, err)

	require.NoError(t, m.DeleteScope(ctx, scope))
	// DeleteScope removes the ephemeral entry's index per-entry and leaves the
	// append-only (audit) entry's index untouched — no whole-scope index wipe, so
	// the audit stays searchable after the session is gone.
	assert.Equal(t, []string{"m-1"}, sp.deleted, "only the ephemeral entry's index is removed")
	assert.Empty(t, sp.scopesDel, "DeleteScope no longer does a whole-scope index wipe")
}

func TestLocal_SearchNoProviders(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	b := &fakeBackend{}
	m := memory.NewLocal(b)
	_, err := m.Search(context.Background(), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
	})
	require.ErrorIs(t, err, memory.ErrNoSearchProviders)
}
