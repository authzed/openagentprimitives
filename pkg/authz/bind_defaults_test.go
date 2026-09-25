package authz_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope" // register Kind
)

func entitiesFor(rt, perm string, defaults ...string) []authz.BoundEntitySpec {
	return []authz.BoundEntitySpec{
		{ResourceType: rt, Permission: perm, Defaults: defaults},
	}
}

func bindSession() authz.SessionRef { return authz.SessionRef{Namespace: "ns", Name: "n"} }

func allowAll() authz.Checker {
	return authz.CheckerFunc(func(_ context.Context, _ authz.Permission, _ authz.Inputs) authz.Result {
		return authz.Result{Outcome: authz.OutcomeAllowed}
	})
}

func systemCtx() context.Context {
	return memory.WithSystemApproval(context.Background(), "test")
}

func TestBindClassDefaults_AllowedDefaultsGrantSlotsAndWriteSessionScope(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/n"}
	entities := entitiesFor("github_repo", "read", "demo-org/demo-repo", "demo-org/internal")
	w := &recordingRelWriter{}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	require.NoError(t, authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(), entities, allowAll(), w, "alice", now, 0))

	// The authorization: one slot grant per allowed default, pointing from the
	// resource at the session.
	require.Len(t, w.wrote, 2, "each allowed default must bind a slot grant")
	for _, rel := range w.wrote {
		assert.Equal(t, "github_repo", rel.ResourceType)
		assert.Equal(t, authz.SlotGrantRelationName("read"), rel.Relation,
			"the relation carries the permission, so a read grant cannot satisfy write")
		assert.Equal(t, "agentsession", rel.SubjectType)
		assert.Equal(t, "ns/n", rel.SubjectID)
	}

	// The Layer-2 narrowing — a separate gate, not a transitional duplicate.
	// See TestBindClassDefaults_ScopeWriteIsLoadBearing below.
	sc, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
	require.NoError(t, err)
	require.True(t, ok, "session_scope entry must exist after BindClassDefaults")

	require.Len(t, sc.Resources, 1, "one ScopeResource entry for github_repo")
	r := sc.Resources[0]
	assert.Equal(t, "github_repo", r.ResourceType)
	assert.ElementsMatch(t, []string{"demo-org/demo-repo", "demo-org/internal"}, r.IDs)
	assert.Equal(t, "default", string(r.Source))
}

// The ScopeResource write is load-bearing, and this test exists because an
// earlier comment promised to delete it once routeViaSessionGrant was retired.
//
// CheckScopeWithRefs narrows PER TYPE: a type absent from scope.Resources is
// unnarrowed, but the moment ANY binding path adds that type, every ref of it
// must match an ID or pattern or be denied. All four fill sources share
// bindSlots, so a defaults path that granted a slot WITHOUT recording a
// ScopeResource would keep working right up until a query or ask fill touched
// the same type — and then start denying the defaults, with a valid grant in
// hand. The failure would look like an authz bug far from its cause.
func TestBindClassDefaults_ScopeWriteIsLoadBearing(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/n"}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	require.NoError(t, authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(),
		entitiesFor("github_repo", "read", "demo-org/demo-repo"),
		allowAll(), &recordingRelWriter{}, "alice", now, 0))

	// A second source binds a DIFFERENT instance of the SAME type, exactly as a
	// query or ask fill would.
	sc, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
	require.NoError(t, err)
	require.True(t, ok)
	sc.Resources = append(sc.Resources, scope.ScopeResource{
		ResourceType: "github_repo",
		IDs:          []string{"demo-org/from-extract"},
		Source:       scope.SourceExtracted,
	})
	require.NoError(t, sessionscope.Put(systemCtx(), mem, memScope, sc))

	ref := scope.ResourceRef{ResourceType: "github_repo", ID: "demo-org/demo-repo"}
	res := scope.CheckScopeWithRefs(sc, "", nil, []scope.ResourceRef{ref}, nil)
	assert.True(t, res.OK,
		"the default-bound instance must still pass Layer 2 after another source narrows its type")

	// And the counterfactual that makes the point: drop only the default's
	// contribution and the very same ref is denied. That is what removing the
	// scope write would do.
	withoutDefaults := sc
	withoutDefaults.Resources = []scope.ScopeResource{{
		ResourceType: "github_repo",
		IDs:          []string{"demo-org/from-extract"},
		Source:       scope.SourceExtracted,
	}}
	denied := scope.CheckScopeWithRefs(withoutDefaults, "", nil, []scope.ResourceRef{ref}, nil)
	assert.False(t, denied.OK,
		"without the defaults' ScopeResource the same grant-backed ref is denied by scope")
}

func TestBindClassDefaults_DeniedDefaultsGrantNothing(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/n2"}
	entities := entitiesFor("github_repo", "read", "secret/repo")
	deny := authz.CheckerFunc(func(_ context.Context, _ authz.Permission, _ authz.Inputs) authz.Result {
		return authz.Result{Outcome: authz.OutcomeDenied, Message: "no access"}
	})
	w := &recordingRelWriter{}

	require.NoError(t, authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(), entities, deny, w, "alice", time.Now, 0))

	assert.Empty(t, w.wrote, "a default the Check refuses must not become a grant")
	_, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
	require.NoError(t, err)
	assert.False(t, ok, "no session_scope entry when all defaults denied")
}

// TestBindClassDefaults_GrantFailureStillNarrowsScope pins the ordering, which
// is the reverse of the usual "authorize first" instinct for a specific reason.
//
// A grant write fails against any resource whose definition lacks the
// slot_grant relation — a type the schema composer skipped because no fragment
// declares it, or a SpiceDB that is simply unreachable. Aborting the bind there
// would also drop the ScopeResource, and CheckScopeWithRefs reads a type ABSENT
// from scope.Resources as "scope does not narrow this type" — so the abort
// would WIDEN Layer-2. Narrowing has to survive a grant failure; only the error
// propagates.
func TestBindClassDefaults_GrantFailureStillNarrowsScope(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/n-grantfail"}
	entities := entitiesFor("github_repo", "read", "demo-org/demo-repo")
	w := &recordingRelWriter{err: errors.New("relation slot_grant not found under definition github_repo")}

	err := authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(), entities, allowAll(), w, "alice", time.Now, 0)
	require.Error(t, err, "a failed grant write must reach the caller")
	assert.Contains(t, err.Error(), "slot_grant not found")

	sc, ok, gerr := sessionscope.Get(systemCtx(), mem, memScope)
	require.NoError(t, gerr)
	require.True(t, ok, "a resource whose grant write failed must keep its Layer-2 narrowing")
	assert.ElementsMatch(t, []string{"demo-org/demo-repo"}, sc.Resources[0].IDs)
}

func TestBindClassDefaults_Idempotent(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/n3"}
	entities := entitiesFor("github_repo", "read", "demo-org/demo-repo")
	w := &recordingRelWriter{}

	require.NoError(t, authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(), entities, allowAll(), w, "alice", time.Now, 0))
	require.NoError(t, authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(), entities, allowAll(), w, "alice", time.Now, 0))

	sc, _, _ := sessionscope.Get(systemCtx(), mem, memScope)
	require.Len(t, sc.Resources, 1)
	assert.ElementsMatch(t, []string{"demo-org/demo-repo"}, sc.Resources[0].IDs, "second call must not duplicate IDs")
	// Grants are TOUCHed, so a repeat write is a no-op in SpiceDB rather than
	// something this function has to suppress.
	assert.Len(t, w.wrote, 2, "the same grant is re-TOUCHed rather than skipped")
}

func TestBindClassDefaults_NoOpInputs(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	cases := []struct {
		name     string
		entities []authz.BoundEntitySpec
		chk      authz.Checker
		subject  string
	}{
		{name: "nil checker", entities: entitiesFor("github_repo", "read", "foo/bar"), chk: nil, subject: "alice"},
		{name: "empty subject", entities: entitiesFor("github_repo", "read", "foo/bar"), chk: allowAll(), subject: ""},
		{name: "no entities", entities: nil, chk: allowAll(), subject: "alice"},
		{name: "empty entities", entities: []authz.BoundEntitySpec{}, chk: allowAll(), subject: "alice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &recordingRelWriter{}
			err := authz.BindClassDefaults(systemCtx(), mem,
				memory.Scope{Kind: "session", ID: "ns/noop"}, bindSession(), tc.entities, tc.chk, w, tc.subject, time.Now, 0)
			assert.NoError(t, err)
			assert.Empty(t, w.wrote, "a no-op input must not grant anything")
		})
	}
}

// TestBindClassDefaults_NoWriter_StillBindsScope keeps the degraded path
// explicit: with no relationship writer the defaults authorize nothing, but
// the call must not fail the session start.
func TestBindClassDefaults_NoWriter_StillBindsScope(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/n-nowriter"}
	entities := entitiesFor("github_repo", "read", "demo-org/demo-repo")

	require.NoError(t, authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(), entities, allowAll(), nil, "alice", time.Now, 0))

	sc, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
	require.NoError(t, err)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"demo-org/demo-repo"}, sc.Resources[0].IDs)
}

// TestBindClassDefaults_FillFromExcludingDefault_BindsNothing is the gate that
// makes the declaration load-bearing. Before it, fillFrom validated and then
// governed nothing: a slot could name [ask] and still have its class-pinned IDs
// bound at session start, which is the opposite of what the field says.
func TestBindClassDefaults_FillFromExcludingDefault_BindsNothing(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/n-nofill"}
	entities := entitiesFor("github_repo", "read", "demo-org/demo-repo")
	entities[0].FillFrom = []string{"ask"}
	w := &recordingRelWriter{}

	require.NoError(t, authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(), entities, allowAll(), w, "alice", time.Now, 0))

	assert.Empty(t, w.wrote, "a slot that excludes the default source must grant nothing")
	_, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
	require.NoError(t, err)
	assert.False(t, ok, "and must not narrow scope to instances it never bound")
}

// TestBindClassDefaults_FillFromIncludingDefault_StillBinds is the other half:
// the gate must not become a blanket refusal, and an unset list must keep
// working for every class authored before the field existed.
func TestBindClassDefaults_FillFromIncludingDefault_StillBinds(t *testing.T) {
	cases := []struct {
		name     string
		fillFrom []string
	}{
		{name: "fillFrom names default: binds", fillFrom: []string{"default", "ask"}},
		{name: "fillFrom unset (pre-field class): binds", fillFrom: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			memScope := memory.Scope{Kind: "session", ID: "ns/n-" + tc.name}
			entities := entitiesFor("github_repo", "read", "demo-org/demo-repo")
			entities[0].FillFrom = tc.fillFrom
			w := &recordingRelWriter{}

			require.NoError(t, authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(), entities, allowAll(), w, "alice", time.Now, 0))

			assert.Len(t, w.wrote, 1, "the default must still bind a slot grant")
			sc, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
			require.NoError(t, err)
			require.True(t, ok)
			assert.ElementsMatch(t, []string{"demo-org/demo-repo"}, sc.Resources[0].IDs)
		})
	}
}

// TestBindClassDefaults_DefaultRunsThroughTheDeclaredTransformChain is the
// live bug this closes: a pinned default that is a raw URL must reach SpiceDB
// as the DERIVED id, not the raw value — dispatch-time Checks compute the id
// via the same chain (authz.ResolveResourceID → ApplyTransforms), so a grant
// written under the raw value is a grant the dispatch-time Check never finds.
func TestBindClassDefaults_DefaultRunsThroughTheDeclaredTransformChain(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/n-transform"}
	entities := entitiesFor("git_repo", "push", "https://github.com/acme/app")
	entities[0].ValueTransforms = []string{"normalize_url", "spicedb_escape"}
	w := &recordingRelWriter{}

	require.NoError(t, authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(), entities, allowAll(), w, "alice", time.Now, 0))

	require.Len(t, w.wrote, 1)
	assert.Equal(t, "https=3A//github=2Ecom/acme/app", w.wrote[0].ResourceID,
		"the grant must carry the id the dispatch-time Check will compute, not the raw URL")
}

// A default value the declared chain cannot derive is skipped rather than
// bound raw or failing the whole class — a class-authoring mistake in one
// default must not take down every other default's binding.
func TestBindClassDefaults_UndrivableDefaultIsSkippedNotBoundRaw(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/n-baddefault"}
	entities := entitiesFor("git_repo", "push", "https://github.com/acme/app")
	entities[0].ValueTransforms = []string{"no_such_transform"}
	w := &recordingRelWriter{}

	require.NoError(t, authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(), entities, allowAll(), w, "alice", time.Now, 0))

	assert.Empty(t, w.wrote, "an undrivable default must bind nothing, not the raw value")
}

// TestBindClassDefaults_GatesPerSlot_NotPerClass: one excluded slot must not
// suppress a sibling that does allow defaults.
func TestBindClassDefaults_GatesPerSlot_NotPerClass(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/n-mixed"}
	entities := []authz.BoundEntitySpec{
		{ResourceType: "github_repo", Permission: "read", Defaults: []string{"demo-org/demo-repo"}, FillFrom: []string{"default"}},
		{ResourceType: "http_target", Permission: "reachable", Defaults: []string{"hash-abc"}, FillFrom: []string{"ask"}},
	}
	w := &recordingRelWriter{}

	require.NoError(t, authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(), entities, allowAll(), w, "alice", time.Now, 0))

	require.Len(t, w.wrote, 1, "only the slot that allows defaults binds")
	assert.Equal(t, "github_repo", w.wrote[0].ResourceType)
}
