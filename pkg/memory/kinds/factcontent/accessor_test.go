package factcontent_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/envelopefact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"

	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// newMem builds a real in-memory facade for accessor tests, plus a ctx
// carrying a system approval. A fake memory.Memory would assert nothing
// here: the whole point of these tests is exercising the REAL append-only
// door (pkg/memory/facade.go), not a stand-in for it. The approval is
// likewise real, not incidental — Query's ReadMemory door is checked
// unconditionally (facade.go's Local.Query), so a bare context.Background()
// fails ForSubject before ever reaching the append-only question these tests
// are about; every accessor test in this tree (toolcatalog, triggerdelivery)
// mints one the same way.
func newMem(t *testing.T) (context.Context, memory.Memory, memory.Scope) {
	t.Helper()
	return memory.WithSystemApproval(context.Background(), "test"),
		memory.NewLocal(inmem.NewBackend()),
		memory.Scope{Kind: "session", ID: "default/demo-session"}
}

// oneObservation is the shape a single `gh pr view` produces: two subjects,
// one fact, co-derived.
func oneObservation(cross bool) factcontent.Observation {
	return factcontent.Observation{
		Subjects: []factcontent.Subject{
			{ResourceType: "git_commit", ResourceID: "ba03f5969a"},
			{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#6"},
		},
		Facts:  map[string]any{"is_cross_repository": cross},
		Source: factcontent.Source{ToolName: "gitlike_gh", ToolUseID: "toolu_01"},
	}
}

func TestRecordFansOutAcrossEverySubject(t *testing.T) {
	ctx, m, scope := newMem(t)

	require.NoError(t, factcontent.Record(ctx, m, scope,
		observedfact.KindName, observedfact.IDPrefix, oneObservation(false)))

	// Both subjects carry the fact, because both were named by the SAME
	// payload. This is co-derivation: the fact is not filed once and looked up
	// by an id derived somewhere else.
	commit, err := factcontent.ForSubject(ctx, m, scope, observedfact.KindName, "git_commit", "ba03f5969a")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"is_cross_repository": false}, commit)

	pr, err := factcontent.ForSubject(ctx, m, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"is_cross_repository": false}, pr)
}

func TestForSubjectIsEmptyForAnUnobservedSubject(t *testing.T) {
	ctx, m, scope := newMem(t)
	require.NoError(t, factcontent.Record(ctx, m, scope,
		observedfact.KindName, observedfact.IDPrefix, oneObservation(false)))

	// THE laundering case, at the storage layer. Observing PR #6 says nothing
	// about PR #5. An empty map here is what a precondition later reads as
	// "undetermined" — never as "false".
	other, err := factcontent.ForSubject(ctx, m, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#5")
	require.NoError(t, err)
	assert.Empty(t, other, "a fact about one PR must not answer for another")
}

func TestRecordIsIdempotentButRefusesContradiction(t *testing.T) {
	ctx, m, scope := newMem(t)

	require.NoError(t, factcontent.Record(ctx, m, scope,
		observedfact.KindName, observedfact.IDPrefix, oneObservation(false)))

	// Same value again: a redelivery, or the agent calling the tool twice.
	require.NoError(t, factcontent.Record(ctx, m, scope,
		observedfact.KindName, observedfact.IDPrefix, oneObservation(false)),
		"an identical re-record is an idempotent no-op")

	// A DIFFERENT value for the same (subject, name) is an attempt to overwrite
	// an inconvenient fact. It must fail loudly, not win.
	err := factcontent.Record(ctx, m, scope,
		observedfact.KindName, observedfact.IDPrefix, oneObservation(true))
	require.Error(t, err)
	assert.ErrorIs(t, err, memory.ErrAppendOnlyConflict)

	// And the FIRST value stands. Write-once means the value is fixed, not that
	// the last writer wins.
	pr, ferr := factcontent.ForSubject(ctx, m, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, ferr)
	assert.Equal(t, map[string]any{"is_cross_repository": false}, pr)
}

// TestRecordToleratesEqualValueFromADifferentObservation is RULING T1-A's test.
//
// Two separate `gh pr view` calls on the same PR produce the same VALUE but
// different provenance — a new ObservationID and a new ToolUseID each time.
// The facade's own door would call that a conflict, because it compares the
// whole entry. Record must compare the value instead, so an ordinary repeat
// observation is a no-op rather than an error the agent cannot act on.
func TestRecordToleratesEqualValueFromADifferentObservation(t *testing.T) {
	ctx, m, scope := newMem(t)

	first := oneObservation(false)
	first.ObservationID = "obs-1"
	first.Source = factcontent.Source{ToolName: "gitlike_gh", ToolUseID: "toolu_01"}
	require.NoError(t, factcontent.Record(ctx, m, scope, observedfact.KindName, observedfact.IDPrefix, first))

	second := oneObservation(false) // same value...
	second.ObservationID = "obs-2"  // ...different provenance
	second.Source = factcontent.Source{ToolName: "gitlike_gh", ToolUseID: "toolu_99"}
	require.NoError(t, factcontent.Record(ctx, m, scope, observedfact.KindName, observedfact.IDPrefix, second),
		"same value from a second observation must be a no-op, not a conflict")

	pr, err := factcontent.ForSubject(ctx, m, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"is_cross_repository": false}, pr)
}

// TestRecordRepeatIsANoOpForEveryValueShape covers the value shapes a fact can
// actually hold, because the two sides of Record's repeat check are NOT the
// same Go types and only a bool hides it.
//
// The stored side has been through JSON — numbers come back float64, lists
// []any, objects map[string]any. The fresh side came straight out of CEL,
// where `size()`, `int()`, arithmetic and integer literals yield int64. A
// direct Go comparison answers "different" for float64(6) against int64(6), so
// an identical re-observation of any non-boolean fact would be reported as a
// contradiction — a false tamper accusation raised by the subsystem whose one
// job is telling a real contradiction from a repeat, and against the explicit
// promise in content.go that "a tool called twice is a no-op".
//
// Every row records the same observation twice and requires the second to be a
// no-op, then requires a genuinely different value at the same (subject, name)
// to still conflict — a comparison loose enough to tolerate everything would
// pass the first half alone.
func TestRecordRepeatIsANoOpForEveryValueShape(t *testing.T) {
	cases := []struct {
		name string
		// value is what CEL would hand Record; different is a contradicting
		// value of the same shape.
		value     any
		different any
		// wantReadBack is value as it comes back out of JSON storage.
		wantReadBack any
	}{
		{
			name:  "bool: the shape every other test already covers",
			value: false, different: true, wantReadBack: false,
		},
		{
			name:  "int64: what CEL size(), int() and integer literals yield",
			value: int64(6), different: int64(7), wantReadBack: float64(6),
		},
		{
			name:  "float64: already JSON's own number type, so it round-trips unchanged",
			value: 1.5, different: 2.5, wantReadBack: 1.5,
		},
		{
			name:  "string: a derived identifier, not free text",
			value: "demo-org/demo-repo", different: "demo-org/other-repo", wantReadBack: "demo-org/demo-repo",
		},
		{
			name:         "list of strings: a CEL comprehension's output",
			value:        []any{"alpha", "beta"},
			different:    []any{"alpha", "gamma"},
			wantReadBack: []any{"alpha", "beta"},
		},
		{
			name:         "list of ints: element types cross the JSON boundary too",
			value:        []any{int64(1), int64(2)},
			different:    []any{int64(1), int64(3)},
			wantReadBack: []any{float64(1), float64(2)},
		},
		{
			name:         "nested map: an int buried one level down still has to match",
			value:        map[string]any{"repo": "demo-org/demo-repo", "count": int64(2)},
			different:    map[string]any{"repo": "demo-org/demo-repo", "count": int64(3)},
			wantReadBack: map[string]any{"repo": "demo-org/demo-repo", "count": float64(2)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, m, scope := newMem(t)
			obs := func(v any) factcontent.Observation {
				o := oneObservation(false)
				o.Facts = map[string]any{"observed_value": v}
				return o
			}

			require.NoError(t, factcontent.Record(ctx, m, scope,
				observedfact.KindName, observedfact.IDPrefix, obs(tc.value)),
				"first observation must record")

			// The same value again, from a second call: different
			// ObservationID and provenance, identical fact.
			second := obs(tc.value)
			second.Source = factcontent.Source{ToolName: "gitlike_gh", ToolUseID: "toolu_99"}
			assert.NoError(t, factcontent.Record(ctx, m, scope,
				observedfact.KindName, observedfact.IDPrefix, second),
				"re-observing the same value must be a no-op, not a conflict")

			err := factcontent.Record(ctx, m, scope,
				observedfact.KindName, observedfact.IDPrefix, obs(tc.different))
			require.Error(t, err, "a different value at the same (subject, name) must still fail")
			assert.ErrorIs(t, err, memory.ErrAppendOnlyConflict)

			// The first value stands, unrewritten by either later call.
			got, ferr := factcontent.ForSubject(ctx, m, scope,
				observedfact.KindName, "github_pr", "demo-org/demo-repo#6")
			require.NoError(t, ferr)
			assert.Equal(t, map[string]any{"observed_value": tc.wantReadBack}, got)
		})
	}
}

// fieldEqualsDroppingBackend is a memory.Backend that does not index content
// and therefore cannot honor FieldEquals — it answers with the WHOLE Kind in
// scope and reports the predicate as dropped, which is the documented
// behavior of any backend declaring Capabilities.ContentSchemas false.
//
// It exists because inmem, the backend every other test here runs on, DOES
// honor FieldEquals. That makes ForSubject's Go-side subject re-check
// unreachable in tests: delete the check and every test on this branch still
// passes, while a real deployment on a non-indexing backend would hand one
// subject's facts to a question asked about another. The re-check's own
// comment calls itself load-bearing; this is what makes that true of the test
// suite too.
type fieldEqualsDroppingBackend struct{ memory.Backend }

func (b fieldEqualsDroppingBackend) Capabilities() memory.Capabilities {
	c := b.Backend.Capabilities()
	c.ContentSchemas = false
	return c
}

func (b fieldEqualsDroppingBackend) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	dropped := len(q.FieldEquals) > 0
	q.FieldEquals = nil
	res, err := b.Backend.Query(ctx, q)
	if err != nil || !dropped {
		return res, err
	}
	res.DroppedPredicates = append(res.DroppedPredicates, "FieldEquals")
	res.Partial = true
	return res, nil
}

// TestForSubjectRechecksTheSubjectWhenTheBackendDropsFieldEquals pins the
// filter that FieldEquals is only an optimization for.
//
// A backend that cannot filter on content returns every fact in the scope.
// Trusting the query would then let a fact observed about PR #5 answer a
// question asked about PR #6 — the laundering the whole mechanism exists to
// prevent, arriving through the storage layer rather than through a
// declaration.
func TestForSubjectRechecksTheSubjectWhenTheBackendDropsFieldEquals(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(fieldEqualsDroppingBackend{inmem.NewBackend()})
	scope := memory.Scope{Kind: "session", ID: "default/demo-session"}

	require.NoError(t, factcontent.Record(ctx, m, scope,
		observedfact.KindName, observedfact.IDPrefix, oneObservation(false)))

	// A DIFFERENT pull request, with a distinctly named fact so a leak is
	// unambiguous rather than a value collision.
	other := factcontent.Observation{
		Subjects: []factcontent.Subject{{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#5"}},
		Facts:    map[string]any{"leaked_from_pr5": true},
		Source:   factcontent.Source{ToolName: "gitlike_gh", ToolUseID: "toolu_05"},
	}
	require.NoError(t, factcontent.Record(ctx, m, scope,
		observedfact.KindName, observedfact.IDPrefix, other))

	// The fake is doing what it claims: the same query, unfiltered by the Go
	// side, really does return other subjects' rows. Without this the test
	// below could pass against a backend that quietly filtered anyway.
	all, err := m.Query(ctx, memory.Query{
		Scope: scope,
		Kinds: []string{observedfact.KindName},
		FieldEquals: []memory.FieldFilter{
			{Path: "resourceType", Value: "github_pr"},
			{Path: "resourceID", Value: "demo-org/demo-repo#6"},
		},
	})
	require.NoError(t, err)
	require.Greater(t, len(all.Entries), 1,
		"the backend fake must hand back more than the requested subject's row, or this test proves nothing")

	got, err := factcontent.ForSubject(ctx, m, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"is_cross_repository": false}, got,
		"only the requested subject's facts may be returned, whatever the backend handed over")
}

// TestKindWrappersSupplyTheirOwnKeying is RULING T1-C's test: a caller that
// never passes (kindName, idPrefix) cannot mismatch them, and the two Kinds
// stay in genuinely separate namespaces.
func TestKindWrappersSupplyTheirOwnKeying(t *testing.T) {
	ctx, m, scope := newMem(t)

	require.NoError(t, observedfact.Record(ctx, m, scope, oneObservation(false)))

	got, err := observedfact.ForSubject(ctx, m, scope, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"is_cross_repository": false}, got)

	// The same subject and name under the OTHER Kind is a different fact. An
	// observed fact must never answer a question asked of an envelope fact —
	// that is the trust grade the two-Kind split exists to keep apart.
	cross, err := envelopefact.ForSubject(ctx, m, scope, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Empty(t, cross, "observed and envelope facts must not share a namespace")

	// And the reverse direction — an envelope fact must not answer a question
	// asked of observedfact either. A fresh subject, never touched by the
	// observedfact.Record above, so an accidental hit here can only be
	// namespace leakage, not a stale value from earlier in this test.
	envOnly := factcontent.Observation{
		Subjects: []factcontent.Subject{{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#9"}},
		Facts:    map[string]any{"is_cross_repository": true},
		Source:   factcontent.Source{ChannelKind: "github", Event: "pull_request"},
	}
	require.NoError(t, envelopefact.Record(ctx, m, scope, envOnly))

	envFacts, err := envelopefact.ForSubject(ctx, m, scope, "github_pr", "demo-org/demo-repo#9")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"is_cross_repository": true}, envFacts)

	leaked, err := observedfact.ForSubject(ctx, m, scope, "github_pr", "demo-org/demo-repo#9")
	require.NoError(t, err)
	assert.Empty(t, leaked, "an envelope fact must not answer a question asked of observedfact")
}

// TestSubjectsEnumeratesOneTypeAndDeduplicates: the read a binder needs. A
// subject appears once however many facts were recorded about it, and only
// subjects of the requested type come back — a git_commit must never be
// proposed for a github_pr slot.
func TestSubjectsEnumeratesOneTypeAndDeduplicates(t *testing.T) {
	ctx, m, scope := newMem(t)

	// One observation naming two types, then a SECOND observation adding
	// another fact about one of the same subjects: two entries, one subject.
	require.NoError(t, factcontent.Record(ctx, m, scope,
		observedfact.KindName, observedfact.IDPrefix, oneObservation(false)))
	require.NoError(t, factcontent.Record(ctx, m, scope,
		observedfact.KindName, observedfact.IDPrefix, factcontent.Observation{
			Subjects: []factcontent.Subject{{ResourceType: "git_commit", ResourceID: "ba03f5969a"}},
			Facts:    map[string]any{"is_signed": true},
		}))
	require.NoError(t, factcontent.Record(ctx, m, scope,
		observedfact.KindName, observedfact.IDPrefix, factcontent.Observation{
			Subjects: []factcontent.Subject{{ResourceType: "git_commit", ResourceID: "0f1e2d3c4b"}},
			Facts:    map[string]any{"is_signed": false},
		}))

	commits, err := factcontent.SubjectsOfTypes(ctx, m, scope, observedfact.KindName, []string{"git_commit"})
	require.NoError(t, err)
	assert.Equal(t, []factcontent.Subject{
		{ResourceType: "git_commit", ResourceID: "0f1e2d3c4b"},
		{ResourceType: "git_commit", ResourceID: "ba03f5969a"},
	}, commits, "two distinct commits, sorted, each named once despite three fact entries")

	prs, err := factcontent.SubjectsOfTypes(ctx, m, scope, observedfact.KindName, []string{"github_pr"})
	require.NoError(t, err)
	assert.Equal(t, []factcontent.Subject{{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#6"}}, prs)
}

// The two Kinds are separate namespaces here as everywhere else: enumerating
// one must not surface the other's subjects. Reading across them would hand a
// caller a signed-envelope trust grade for a subject only a tool result named.
func TestSubjectsDoesNotCrossTheKindBoundary(t *testing.T) {
	ctx, m, scope := newMem(t)

	require.NoError(t, factcontent.Record(ctx, m, scope,
		observedfact.KindName, observedfact.IDPrefix, factcontent.Observation{
			Subjects: []factcontent.Subject{{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#6"}},
			Facts:    map[string]any{"is_cross_repository": false},
		}))
	require.NoError(t, factcontent.Record(ctx, m, scope,
		envelopefact.KindName, envelopefact.IDPrefix, factcontent.Observation{
			Subjects: []factcontent.Subject{{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#7"}},
			Facts:    map[string]any{"head_is_fork": true},
		}))

	obs, err := factcontent.SubjectsOfTypes(ctx, m, scope, observedfact.KindName, []string{"github_pr"})
	require.NoError(t, err)
	assert.Equal(t, []factcontent.Subject{{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#6"}}, obs)

	env, err := factcontent.SubjectsOfTypes(ctx, m, scope, envelopefact.KindName, []string{"github_pr"})
	require.NoError(t, err)
	assert.Equal(t, []factcontent.Subject{{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#7"}}, env)
}

// Nothing observed is an empty answer, not an error: a binder reads it as
// "no candidate", which is what holds the slot closed.
func TestSubjectsEmptyWhenNothingObserved(t *testing.T) {
	ctx, m, scope := newMem(t)

	got, err := factcontent.SubjectsOfTypes(ctx, m, scope, observedfact.KindName, []string{"git_commit"})
	require.NoError(t, err)
	assert.Empty(t, got)
}

// An empty type list is REFUSED rather than widened to the whole scope. A
// caller that passed an empty variable by accident would otherwise be handed
// subjects of every type in the session and bind instances of types it never
// asked about.
func TestSubjectsRefusesAnEmptyTypeList(t *testing.T) {
	ctx, m, scope := newMem(t)

	_, err := factcontent.SubjectsOfTypes(ctx, m, scope, observedfact.KindName, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no resource types named")
}

// The wrappers exist so no caller outside the Kind packages passes a
// (KindName, IDPrefix) pair by hand — the pair that is NOT malformed, and
// would silently read the other namespace, is the one they close.
func TestKindWrappersReadTheirOwnNamespace(t *testing.T) {
	ctx, m, scope := newMem(t)

	require.NoError(t, observedfact.Record(ctx, m, scope, factcontent.Observation{
		Subjects: []factcontent.Subject{{ResourceType: "git_commit", ResourceID: "ba03f5969a"}},
		Facts:    map[string]any{"is_cross_repository": false},
	}))

	obs, err := observedfact.SubjectsOfTypes(ctx, m, scope, []string{"git_commit"})
	require.NoError(t, err)
	assert.Equal(t, []factcontent.Subject{{ResourceType: "git_commit", ResourceID: "ba03f5969a"}}, obs)

	env, err := envelopefact.SubjectsOfTypes(ctx, m, scope, []string{"git_commit"})
	require.NoError(t, err)
	assert.Empty(t, env, "an observed fact must not be visible through the envelope namespace")
}

// The list form is the one the binder actually calls: several declared types in
// ONE query, still confined to the types named. The type absent from the list
// must not come back — that confinement is what keeps a git_commit fact from
// proposing a candidate for a slot of another type, and with more than one type
// the indexed predicate cannot be used, so the Go-side filter is carrying it
// alone here.
func TestSubjectsOfTypesReadsSeveralTypesAtOnceAndStillExcludesTheRest(t *testing.T) {
	ctx, m, scope := newMem(t)

	require.NoError(t, factcontent.Record(ctx, m, scope,
		observedfact.KindName, observedfact.IDPrefix, oneObservation(false)))
	require.NoError(t, factcontent.Record(ctx, m, scope,
		observedfact.KindName, observedfact.IDPrefix, factcontent.Observation{
			Subjects: []factcontent.Subject{{ResourceType: "linear_issue", ResourceID: "ENG-1"}},
			Facts:    map[string]any{"is_confidential": true},
		}))

	got, err := factcontent.SubjectsOfTypes(ctx, m, scope, observedfact.KindName,
		[]string{"git_commit", "github_pr"})
	require.NoError(t, err)
	assert.Equal(t, []factcontent.Subject{
		{ResourceType: "git_commit", ResourceID: "ba03f5969a"},
		{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#6"},
	}, got, "both named types, sorted by (type, id); the unnamed linear_issue is not proposed")
}
