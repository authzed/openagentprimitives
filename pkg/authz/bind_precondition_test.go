package authz_test

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/envelopefact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// mustCompile is the admission-time compile every path already ran before a
// precondition reaches a BoundEntitySpec. require, not assert: a test whose
// predicate did not compile would go on to assert that a candidate was held,
// and pass for the wrong reason.
//
// The two messages are filled with recognizable placeholders because the CRD
// requires them and this file's subject — whether the candidate BINDS — does
// not read them. What the agent is told about a hold is dispatch's question,
// pinned in pkg/authz/hooks.
func mustCompile(t *testing.T, expr string) precondition.Rule {
	t.Helper()
	c, err := precondition.Compile(expr)
	require.NoError(t, err, "the predicate under test must itself be valid")
	return precondition.Rule{
		Compiled:         c,
		UndeterminedHint: "fixture hint: " + expr,
		RefusalMessage:   "fixture refusal: " + expr,
	}
}

// gatedSlot is a slot fillable only by an observation and gated on the facts
// that observation carries — the design's fork gate, in the smallest shape that
// still exercises the real path.
func gatedSlot(rt, perm string, requires ...precondition.Rule) authz.BoundEntitySpec {
	return authz.BoundEntitySpec{
		ResourceType: rt,
		Permission:   perm,
		FillFrom:     []string{"observed"},
		Requires:     requires,
	}
}

// recordEnvelope writes what a signed webhook's TriggerFacts records: the same
// co-derivation as an observed fact, differing only in provenance — which is a
// trust grade, and is why a predicate over facts.envelope.* must not be
// answerable from the observed namespace.
func recordEnvelope(t *testing.T, mem memory.Memory, memScope memory.Scope, rt, id, name string, value any) {
	t.Helper()
	require.NoError(t, envelopefact.Record(systemCtx(), mem, memScope, factcontent.Observation{
		Subjects: []factcontent.Subject{{ResourceType: rt, ResourceID: id}},
		Facts:    map[string]any{name: value},
		Source:   factcontent.Source{ChannelKind: "github", Event: "pull_request"},
	}))
}

// TestBindPrecondition_HoldsACandidateOutOfItsSlot is the gate.
//
// Every row drives the REAL promotion path against a REAL in-memory
// memory.Memory seeded through the REAL Record accessors, because the read path
// is exactly what is under test: a fake that answered "here are the facts"
// would pass whether or not the filter ever managed to look one up.
//
// Each row asserts POSITIVELY on the outcome it expects — a bound row requires
// the grant that was written, a held row requires that none was. Asserting only
// "no error" would pass for a candidate that was never proposed at all, which
// is how a filter test ends up green for a reason unrelated to its name.
func TestBindPrecondition_HoldsACandidateOutOfItsSlot(t *testing.T) {
	const notAFork = `facts.observed.is_cross_repository == false`

	cases := []struct {
		name string
		// seed writes the facts the session has recorded so far. nil means
		// nothing has been observed about the candidate — which is the
		// undetermined row, and it must NOT be spelled as "the fact is false".
		seed func(t *testing.T, mem memory.Memory, memScope memory.Scope)
		slot func(t *testing.T) authz.BoundEntitySpec
		// wantBound is the raw id the candidate must occupy the slot with, or
		// empty when the precondition must hold it out.
		wantBound string
		// why states, in the failure message, which verdict the row is about.
		why string
	}{
		{
			name: "fact present and the predicate is true (Satisfied): the candidate binds",
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", false)
			},
			slot: func(t *testing.T) authz.BoundEntitySpec {
				return gatedSlot("git_commit", "read", mustCompile(t, notAFork))
			},
			wantBound: "ba03f5969a",
			why:       "a Satisfied precondition must not block a candidate the requester can reach",
		},
		{
			name: "fact present and the predicate is false (Refused): no binding",
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				// The motivating case: the head lives on a fork, so reviewing it
				// means checking out untrusted code. The slot never binds, and
				// every permissioned call against it then fails closed through
				// the ordinary authorization path.
				recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", true)
			},
			slot: func(t *testing.T) authz.BoundEntitySpec {
				return gatedSlot("git_commit", "read", mustCompile(t, notAFork))
			},
			why: "a Refused precondition must hold the candidate out of the slot",
		},
		{
			name: "no fact recorded at all (Undetermined): no binding",
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				// A fact about a DIFFERENT object of the same type. The subject
				// exists, so a candidate IS proposed and reaches the filter —
				// without this the row would pass merely because nothing was
				// ever proposed, which is the vacuous shape this test is
				// written to avoid.
				recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", false)
			},
			slot: func(t *testing.T) authz.BoundEntitySpec {
				// Gated on a fact NOBODY has recorded for this subject. Not
				// "recorded false" — unrecorded, which is the third verdict and
				// must never be collapsed into either of the other two.
				return gatedSlot("git_commit", "read", mustCompile(t, `facts.observed.head_is_signed == true`))
			},
			why: "an Undetermined precondition must hold the candidate out; undetermined never binds",
		},
		{
			name: "a slot declaring NO requires binds exactly as before",
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", true)
			},
			// Deliberately seeded with is_cross_repository=true — the value the
			// Refused row uses. An ungated slot must be indifferent to it: this
			// is the regression guard for every class that predates
			// preconditions, and it would fail if the filter were ever made to
			// apply a default rule to a slot that declared none.
			slot:      func(t *testing.T) authz.BoundEntitySpec { return gatedSlot("git_commit", "read") },
			wantBound: "ba03f5969a",
			why:       "a slot with no requires must be unaffected by the facts recorded around it",
		},
		{
			name: "a precondition over an ENVELOPE fact decides too",
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				recordEnvelope(t, mem, memScope, "github_pr", "demo-org/demo-repo#6", "head_is_fork", false)
			},
			slot: func(t *testing.T) authz.BoundEntitySpec {
				return gatedSlot("github_pr", "fetch", mustCompile(t, `facts.envelope.head_is_fork == false`))
			},
			wantBound: "demo-org/demo-repo#6",
			why:       "a signed delivery's fact must satisfy a gate the same way a tool result's does",
		},
		{
			name: "an ENVELOPE fact that refuses holds the candidate",
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				recordEnvelope(t, mem, memScope, "github_pr", "demo-org/demo-repo#6", "head_is_fork", true)
			},
			slot: func(t *testing.T) authz.BoundEntitySpec {
				return gatedSlot("github_pr", "fetch", mustCompile(t, `facts.envelope.head_is_fork == false`))
			},
			why: "the envelope namespace must be able to close the gate, not only open it",
		},
		{
			// The trust-grade boundary. An observed fact of the same NAME must
			// not answer a predicate written against the envelope namespace: a
			// fact the agent shaped a call to produce is a weaker claim than one
			// the platform derived from a signed payload, and the two namespaces
			// exist so an author cannot silently receive the wrong grade.
			name: "an OBSERVED fact does not answer an ENVELOPE predicate",
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				require.NoError(t, observedfact.Record(systemCtx(), mem, memScope, factcontent.Observation{
					Subjects: []factcontent.Subject{{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#6"}},
					Facts:    map[string]any{"head_is_fork": false},
					Source:   factcontent.Source{ToolName: "gitlike_gh"},
				}))
			},
			slot: func(t *testing.T) authz.BoundEntitySpec {
				return gatedSlot("github_pr", "fetch", mustCompile(t, `facts.envelope.head_is_fork == false`))
			},
			why: "provenance is a trust grade; the observed namespace must read as undetermined here",
		},
		{
			// Conjunction: one reference satisfied, the other unrecorded. CEL
			// would short-circuit `a && b` on a false `a`, so the presence check
			// has to precede evaluation for this to stay undetermined rather
			// than becoming a decision nobody's observation cleared.
			name: "a conjunction with one unrecorded reference stays held",
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", false)
			},
			slot: func(t *testing.T) authz.BoundEntitySpec {
				return gatedSlot("git_commit", "read",
					mustCompile(t, `facts.observed.is_cross_repository == false && facts.envelope.head_is_fork == false`))
			},
			why: "every referenced fact must be recorded before the predicate may decide",
		},
		{
			// Two preconditions, both compiled, one Refused. ALL must be
			// Satisfied, so a single refusal holds the candidate even though the
			// other predicate is true.
			name: "several requires: one Refused holds the candidate",
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", true)
			},
			slot: func(t *testing.T) authz.BoundEntitySpec {
				return gatedSlot("git_commit", "read",
					mustCompile(t, `slot.resourceType == "git_commit"`),
					mustCompile(t, notAFork))
			},
			why: "requires is a conjunction across entries; one refusal is a hold",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			memScope := memory.Scope{Kind: "session", ID: "ns/" + tc.name}
			tc.seed(t, mem, memScope)
			w := &recordingRelWriter{}

			// Built once: the row under test is a single slot, and calling the
			// factory again to read a field back would recompile its predicates
			// and assert against a spec the run never saw.
			slot := tc.slot(t)

			require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, bindSession(),
				[]authz.BoundEntitySpec{slot}, allowAll(), w, "alice", time.Now, 0))

			if tc.wantBound == "" {
				assert.Empty(t, w.wrote, tc.why)
				// The Layer-2 half must be held back too. A ScopeResource
				// written for an instance that never got a grant would record
				// the session as narrowed to something it was refused.
				_, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
				require.NoError(t, err)
				assert.False(t, ok, "a held candidate must not reach the scope document either")
				return
			}

			// Positive assertion, deliberately: the row proves a binding
			// HAPPENED, so it cannot pass because nothing was ever proposed.
			require.Len(t, w.wrote, 1, tc.why)
			assert.Equal(t, tc.wantBound, w.wrote[0].ResourceID, tc.why)
			assert.Equal(t, authz.SlotGrantRelationName(slot.Permission), w.wrote[0].Relation,
				"the grant must carry the slot's permission")

			sc, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
			require.NoError(t, err)
			require.True(t, ok, "a bound candidate narrows scope as well as granting")
			assert.ElementsMatch(t, []string{tc.wantBound}, sc.Resources[0].IDs)
		})
	}
}

// TestBindPrecondition_LooksFactsUpByTheRawID is the P-2 contract, asserted
// where it can actually be got wrong.
//
// The slot declares a transform chain, so its object id and its raw id DIFFER —
// `demo-org/demo-repo#6` becomes something `#`-free for SpiceDB. Facts are
// keyed by the raw form. A filter that looked them up by the derived id would
// find nothing, report undetermined, and hold a candidate whose gating fact is
// sitting right there — failing closed permanently and invisibly, which reads
// exactly like an observation that never happened.
//
// Without the transform chain this test would pass either way, which is why the
// chain is the point of the fixture rather than incidental to it.
func TestBindPrecondition_LooksFactsUpByTheRawID(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/raw-id-lookup"}
	recordEnvelope(t, mem, memScope, "github_pr", "demo-org/demo-repo#6", "head_is_fork", false)

	slot := gatedSlot("github_pr", "fetch", mustCompile(t, `facts.envelope.head_is_fork == false`))
	slot.ValueTransforms = []string{"spicedb_escape"}

	derived, err := authz.NewObjectID("demo-org/demo-repo#6", slot.ValueTransforms)
	require.NoError(t, err)
	require.NotEqual(t, "demo-org/demo-repo#6", derived.String(),
		"the fixture is only meaningful while the transform actually changes the id")

	w := &recordingRelWriter{}
	require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{slot}, allowAll(), w, "alice", time.Now, 0))

	require.Len(t, w.wrote, 1,
		"the gating fact is recorded under the RAW id; a lookup by the derived id would hold this candidate forever")
	assert.Equal(t, derived.String(), w.wrote[0].ResourceID,
		"SpiceDB is still asked about the DERIVED id — only the fact lookup uses the raw one")
}

// TestBindPrecondition_GatesEveryFillSource pins that the filter is not wired
// into one source and forgotten in the others.
//
// The class-defaults path is the one that predates every other fill source and
// the one an existing class is most likely to be using, so a precondition that
// only governed the observed source would leave the oldest route ungated while
// every scenario written for the new one passed.
func TestBindPrecondition_GatesEveryFillSource(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/defaults-gated"}
	recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", true) // a fork: refused

	gated := authz.BoundEntitySpec{
		ResourceType: "git_commit",
		Permission:   "read",
		Defaults:     []string{"ba03f5969a"},
		Requires:     []precondition.Rule{mustCompile(t, `facts.observed.is_cross_repository == false`)},
	}
	w := &recordingRelWriter{}
	require.NoError(t, authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{gated}, allowAll(), w, "alice", time.Now, 0))
	assert.Empty(t, w.wrote, "a class-pinned default is a candidate like any other and its precondition governs it")

	// And the counterfactual, so the row above cannot pass because the defaults
	// path binds nothing at all: the same default, ungated, binds.
	ungated := gated
	ungated.Requires = nil
	w2 := &recordingRelWriter{}
	require.NoError(t, authz.BindClassDefaults(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{ungated}, allowAll(), w2, "alice", time.Now, 0))
	require.Len(t, w2.wrote, 1, "without the precondition the very same default binds")
	assert.Equal(t, "ba03f5969a", w2.wrote[0].ResourceID)
}

// TestBindPrecondition_ApprovalWaivesTheGate pins the design's WAIVER path.
//
// A Refused verdict is not the end: a human may still consent to it from a card
// that explains what they are consenting to, and BindApproved with
// PreconditionsWaived is what records that consent. Re-applying the gate there
// would make the consent unusable — the card would appear, the human would
// approve, and nothing would happen.
//
// The binding carries the Refused rule AND the raw id, exactly as the plan-gate
// path would, so the ONLY thing keeping the instance bound is
// PreconditionsWaived. That makes this the true counterpart to
// TestBindApproved_planGatePolicyEnforcesPreconditions: same Refused fact, same
// carried rule, opposite policy, opposite outcome.
func TestBindPrecondition_ApprovalWaivesTheGate(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/approval-waives"}
	recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", true) // a fork: Refused

	w := &recordingRelWriter{}
	require.NoError(t, authz.BindApproved(systemCtx(), mem, memScope, bindSession(), w,
		[]authz.SlotBinding{{
			ResourceType: "git_commit",
			ResourceID:   authz.TrustedObjectID("ba03f5969a"),
			RawID:        "ba03f5969a",
			Permission:   "read",
			Requires:     []precondition.Rule{mustCompile(t, `facts.observed.is_cross_repository == false`)},
		}},
		authz.PreconditionsWaived, time.Now().Add(time.Hour), logr.Discard(), time.Now))

	require.Len(t, w.wrote, 1, "the waiver binds the instance a human cleared, Refused precondition and all")
	assert.Equal(t, "ba03f5969a", w.wrote[0].ResourceID)
}

// TestBindApproved_planGatePolicyEnforcesPreconditions is the confused-deputy
// gap this task closes, at the pkg/authz layer.
//
// A plan-phase approval consents to the PHASE, not to the risk a precondition
// guards. The plan-gate path binds through BindApproved with
// EnforcePreconditions and carries each binding's compiled Requires + raw id, so
// an instance the phase named whose gate is Refused must be DROPPED — left
// unbound to escalate to the waiver card — not silently bound. This is the exact
// same input as TestBindPrecondition_ApprovalWaivesTheGate above, with the
// policy flipped, and the outcome flips with it.
//
// The regression row proves the enforcing default did not wedge the ordinary
// phase: the SAME call, with the gate Satisfied, binds as before.
func TestBindApproved_planGatePolicyEnforcesPreconditions(t *testing.T) {
	const notAFork = `facts.observed.is_cross_repository == false`

	cases := []struct {
		name      string
		crossRepo bool // the recorded fact: true is a fork (Refused), false satisfies
		wantBound bool
		why       string
	}{
		{
			name:      "Refused gate: EnforcePreconditions drops the instance rather than binding it",
			crossRepo: true,
			wantBound: false,
			why:       "a plan-phase approval must not silently waive a precondition; the instance escalates to the waiver card",
		},
		{
			name:      "Satisfied gate: the ordinary phase binds exactly as before",
			crossRepo: false,
			wantBound: true,
			why:       "enforcing preconditions on the approval path must not wedge a phase whose gate is met (or absent)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			memScope := memory.Scope{Kind: "session", ID: "ns/plangate-enforces-" + tc.name}
			recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", tc.crossRepo)

			w := &recordingRelWriter{}
			require.NoError(t, authz.BindApproved(systemCtx(), mem, memScope, bindSession(), w,
				[]authz.SlotBinding{{
					ResourceType: "git_commit",
					ResourceID:   authz.TrustedObjectID("ba03f5969a"),
					RawID:        "ba03f5969a",
					Permission:   "read",
					Requires:     []precondition.Rule{mustCompile(t, notAFork)},
				}},
				authz.EnforcePreconditions, time.Now().Add(time.Hour), logr.Discard(), time.Now))

			if !tc.wantBound {
				assert.Empty(t, w.wrote, tc.why)
				_, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
				require.NoError(t, err)
				assert.False(t, ok, "a dropped instance must not reach session_scope either")
				return
			}
			require.Len(t, w.wrote, 1, tc.why)
			assert.Equal(t, "ba03f5969a", w.wrote[0].ResourceID, tc.why)
		})
	}
}
