package authz_test

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/envelopefact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// recordObserved writes what a tool's `observes` block records after a
// successful call: one subject, one fact, co-derived from the same result.
//
// The write goes through the real observedfact.Record into a real in-memory
// memory.Memory rather than through a fake, because the read path is what is
// under test here — a stub that answered "these subjects" would pass whether or
// not the promotion ever managed to enumerate a stored fact.
func recordObserved(t *testing.T, mem memory.Memory, memScope memory.Scope, rt, id string, value any) {
	t.Helper()
	require.NoError(t, observedfact.Record(systemCtx(), mem, memScope, factcontent.Observation{
		Subjects: []factcontent.Subject{{ResourceType: rt, ResourceID: id}},
		Facts:    map[string]any{"is_cross_repository": value},
		Source:   factcontent.Source{ToolName: "gitlike_gh"},
	}))
}

// observedSlot is a slot that can ONLY be filled by an observation — the
// declaration the design's fork gate uses.
func observedSlot(rt, perm string) authz.BoundEntitySpec {
	return authz.BoundEntitySpec{ResourceType: rt, Permission: perm, FillFrom: []string{"observed"}}
}

func TestPromoteObservedSlots_BindsCheckedCandidates(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/observed"}
	recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", false)
	w := &pinningFake{}

	require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{observedSlot("git_commit", "read")}, allowAll(), w, "alice", time.Now, 0))

	require.Len(t, w.wrote, 1, "a Checked observed subject must become a slot grant")
	assert.Equal(t, "git_commit", w.wrote[0].ResourceType)
	assert.Equal(t, "ba03f5969a", w.wrote[0].ResourceID)
	assert.Equal(t, authz.SlotGrantRelationName("read"), w.wrote[0].Relation,
		"the relation carries the permission, so a read grant cannot satisfy checkout")

	// The Layer-2 half is not optional: a class whose type is already in
	// scope.Resources narrows per type, so a grant written without the matching
	// ScopeResource would be refused by the scope layer and bind nothing.
	sc, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
	require.NoError(t, err)
	require.True(t, ok, "promotion must also narrow scope to the bound instance")
	assert.Equal(t, "observed", string(sc.Resources[0].Source),
		"the audit trail must say a fact proposed this, not that a person or a default did")
	assert.ElementsMatch(t, []string{"ba03f5969a"}, sc.Resources[0].IDs)
}

// TestPromoteObservedSlots_DropsWhatItMustNotBind covers the filters, each of
// which closes a different hole. A fact is written by whoever observed it and
// decides nothing; every row here is a way a recording could otherwise become
// authority nobody granted.
func TestPromoteObservedSlots_DropsWhatItMustNotBind(t *testing.T) {
	cases := []struct {
		name     string
		factType string
		factID   string
		declared []authz.BoundEntitySpec
		chk      authz.Checker
		wantIDs  []string
	}{
		{
			name:     "type the class does not declare: dropped",
			factType: "git_commit", factID: "ba03f5969a",
			declared: []authz.BoundEntitySpec{observedSlot("github_pr", "fetch")},
			chk:      allowAll(),
		},
		{
			// The row that proves fillFrom governs THIS source. A slot the class
			// pins its own ids for must not be occupied by an object a tool
			// result happened to name — that is the whole point of declaring the
			// route, and without the AllowsFill check it would bind here.
			name:     "declared type whose fillFrom admits no observed binding: dropped",
			factType: "git_commit", factID: "ba03f5969a",
			declared: []authz.BoundEntitySpec{{ResourceType: "git_commit", Permission: "read", FillFrom: []string{"default"}}},
			chk:      allowAll(),
		},
		{
			name:     "requester has no standing: dropped, not escalated",
			factType: "git_commit", factID: "ba03f5969a",
			declared: []authz.BoundEntitySpec{observedSlot("git_commit", "read")},
			chk: authz.CheckerFunc(func(_ context.Context, _ authz.Permission, _ authz.Inputs) authz.Result {
				return authz.Result{Outcome: authz.OutcomeDenied, Message: "not yours"}
			}),
		},
		{
			name:     "declared, observed-fillable and Checked: bound",
			factType: "git_commit", factID: "ba03f5969a",
			declared: []authz.BoundEntitySpec{observedSlot("git_commit", "read")},
			chk:      allowAll(),
			wantIDs:  []string{"ba03f5969a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			memScope := memory.Scope{Kind: "session", ID: "ns/" + tc.name}
			recordObserved(t, mem, memScope, tc.factType, tc.factID, false)
			w := &pinningFake{}

			require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, bindSession(),
				tc.declared, tc.chk, w, "alice", time.Now, 0))

			got := make([]string, 0, len(w.wrote))
			for _, r := range w.wrote {
				got = append(got, r.ResourceID)
			}
			assert.ElementsMatch(t, tc.wantIDs, got)

			if len(tc.wantIDs) > 0 {
				return
			}
			_, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
			require.NoError(t, err)
			assert.False(t, ok, "a dropped candidate must not widen the scope document either")
		})
	}
}

// Two objects observed, one of them a type the class declares. Exactly one
// binding: the walk must propose what the class asked about and nothing else.
// A read that enumerated the Kind and filtered afterwards would still pass the
// single-subject rows above while binding both here.
func TestPromoteObservedSlots_OnlyTheDeclaredTypeBinds(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/observed-two"}
	// One payload, two co-derived subjects — the design's own example.
	recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", false)
	recordObserved(t, mem, memScope, "github_pr", "demo-org/demo-repo#6", false)
	w := &pinningFake{}

	require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{observedSlot("git_commit", "read")}, allowAll(), w, "alice", time.Now, 0))

	require.Len(t, w.wrote, 1)
	assert.Equal(t, "git_commit", w.wrote[0].ResourceType)
	assert.Equal(t, "ba03f5969a", w.wrote[0].ResourceID)
}

// Promotion re-runs after every dispatch round, so binding twice must be a
// no-op rather than a duplicate or a conflict.
func TestPromoteObservedSlots_Idempotent(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/observed-idem"}
	recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", false)
	w := &pinningFake{}
	specs := []authz.BoundEntitySpec{observedSlot("git_commit", "read")}

	require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, bindSession(), specs, allowAll(), w, "alice", time.Now, 0))
	require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, bindSession(), specs, allowAll(), w, "alice", time.Now, 0))

	sc, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, sc.Resources, 1)
	assert.ElementsMatch(t, []string{"ba03f5969a"}, sc.Resources[0].IDs, "re-running must not duplicate IDs")
	assert.Equal(t, "ba03f5969a", w.wrote[0].ResourceID, "the same grant, TOUCHed again")
}

// An envelope fact proposes too. The two Kinds are a trust grade, not two
// mechanisms: a slot gated on facts.envelope.* whose candidate nobody proposed
// would be inert in exactly the way this function exists to prevent.
func TestPromoteObservedSlots_EnvelopeFactAlsoProposes(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/observed-envelope"}
	require.NoError(t, envelopefact.Record(systemCtx(), mem, memScope, factcontent.Observation{
		Subjects: []factcontent.Subject{{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#6"}},
		Facts:    map[string]any{"head_is_fork": true},
		Source:   factcontent.Source{ChannelKind: "github", Event: "pull_request"},
	}))
	w := &pinningFake{}

	require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{observedSlot("github_pr", "fetch")}, allowAll(), w, "alice", time.Now, 0))

	require.Len(t, w.wrote, 1, "a signed delivery's fact must propose its subject the same way a tool result's does")
	assert.Equal(t, "demo-org/demo-repo#6", w.wrote[0].ResourceID)
}

// The same object observed by BOTH Kinds is one candidate, not two: a pull
// request that arrived in a signed delivery and was then read by a tool is the
// ordinary case.
func TestPromoteObservedSlots_SameSubjectInBothKindsBindsOnce(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/observed-both"}
	require.NoError(t, envelopefact.Record(systemCtx(), mem, memScope, factcontent.Observation{
		Subjects: []factcontent.Subject{{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#6"}},
		Facts:    map[string]any{"head_is_fork": true},
	}))
	recordObserved(t, mem, memScope, "github_pr", "demo-org/demo-repo#6", true)
	w := &pinningFake{}

	require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{observedSlot("github_pr", "fetch")}, allowAll(), w, "alice", time.Now, 0))

	assert.Len(t, w.wrote, 1, "one object is one candidate however many Kinds recorded facts about it")
}

// Facts are keyed RAW. The grant must carry the id the dispatch-time Check will
// compute — the pre-transform value put through the slot's own chain — because
// a lookup by the derived form could never have found the fact in the first
// place (spicedb_escape exists because `#` is illegal in an object id).
func TestPromoteObservedSlots_RawSubjectRunsThroughTheDeclaredTransformChain(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/observed-transform"}
	recordObserved(t, mem, memScope, "github_pr", "demo-org/demo-repo#6", false)
	w := &pinningFake{}
	slot := observedSlot("github_pr", "fetch")
	slot.ValueTransforms = []string{"spicedb_escape"}

	require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{slot}, allowAll(), w, "alice", time.Now, 0))

	require.Len(t, w.wrote, 1)
	assert.NotEqual(t, "demo-org/demo-repo#6", w.wrote[0].ResourceID,
		"the raw key a fact is stored under is not a legal SpiceDB object id")
	assert.Equal(t, "demo-org/demo-repo=236", w.wrote[0].ResourceID,
		"the grant must carry the id the dispatch-time Check computes through the same chain")
}

// A subject the declared chain cannot derive is dropped, never bound under its
// raw value.
func TestPromoteObservedSlots_UndrivableSubjectIsDroppedNotBoundRaw(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/observed-badtransform"}
	recordObserved(t, mem, memScope, "github_pr", "demo-org/demo-repo#6", false)
	w := &pinningFake{}
	slot := observedSlot("github_pr", "fetch")
	slot.ValueTransforms = []string{"no_such_transform"}

	require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{slot}, allowAll(), w, "alice", time.Now, 0))

	assert.Empty(t, w.wrote, "an underivable subject must bind nothing, not the raw value")
}

func TestPromoteObservedSlots_NoOpInputs(t *testing.T) {
	specs := []authz.BoundEntitySpec{observedSlot("git_commit", "read")}

	cases := []struct {
		name    string
		specs   []authz.BoundEntitySpec
		chk     authz.Checker
		subject string
		seed    bool
	}{
		{name: "nil checker", specs: specs, chk: nil, subject: "alice", seed: true},
		{name: "empty subject", specs: specs, chk: allowAll(), subject: "", seed: true},
		{name: "no declared slots", specs: nil, chk: allowAll(), subject: "alice", seed: true},
		{name: "nothing observed", specs: specs, chk: allowAll(), subject: "alice", seed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			memScope := memory.Scope{Kind: "session", ID: "ns/noop-" + tc.name}
			if tc.seed {
				recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", false)
			}
			w := &pinningFake{}
			require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, bindSession(),
				tc.specs, tc.chk, w, tc.subject, time.Now, 0))
			assert.Empty(t, w.wrote)
		})
	}
}

// A human waived the fork gate for this commit, and then the agent kept
// working. Promotion re-runs after every dispatch round, so it reaches an
// entry a person already put in scope — and it must not re-tag that entry as
// its own.
//
// The failure this pins is pure audit, which is exactly why it needs a test:
// nothing denies, nothing errors, the grant is fine, and the only trace of a
// human decision quietly turns into "an observation did this". The design gives
// this scenario by name — a reviewer bound to a fork pull request whose gate a
// person waived.
func TestPromoteObservedSlots_DoesNotRetagAHumanApprovedScopeEntry(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/observed-approved"}
	sess := bindSession()
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	// The human's decision, through the real approval binding path — a waiver of
	// the fork gate, so PreconditionsWaived, the same policy the waiver card uses.
	require.NoError(t, authz.BindApproved(systemCtx(), mem, memScope, sess, nil,
		[]authz.SlotBinding{{ResourceType: "git_commit", ResourceID: authz.TrustedObjectID("ba03f5969a"), Permission: "read"}},
		authz.PreconditionsWaived, now().Add(time.Hour), logr.Discard(), now))

	before, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "approved", string(before.Resources[0].Source), "precondition: the human's tag is what we are protecting")

	// Now the agent observes the same commit and promotion runs, twice, the way
	// consecutive dispatch rounds would.
	recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", false)
	specs := []authz.BoundEntitySpec{observedSlot("git_commit", "read")}
	w := &pinningFake{}
	require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, sess, specs, allowAll(), w, "alice", now, 0))
	require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, sess, specs, allowAll(), w, "alice", now, 0))

	// GATE the provenance claim on promotion having actually PROPOSED something.
	// Without this the test passes for free whenever the candidate never reaches
	// the scope write at all — a fillFrom filter dropping it, a Check denying it,
	// a walk that stopped enumerating — none of which is the thing the name
	// promises. The tag is only evidence of non-downgrade if a downgrade was on
	// the table.
	require.NotEmpty(t, w.wrote, "promotion must have bound this instance, or the Source below proves nothing")
	assert.Equal(t, "ba03f5969a", w.wrote[0].ResourceID)

	after, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, after.Resources, 1)
	assert.Equal(t, "approved", string(after.Resources[0].Source),
		"a human decision must not be re-tagged as an observation by a source that re-runs")
	assert.ElementsMatch(t, []string{"ba03f5969a"}, after.Resources[0].IDs)
}

// The mirror: an entry promotion CREATED still carries its own tag, so the
// preservation rule above cannot be satisfied by never writing a Source at all.
func TestPromoteObservedSlots_TagsAnEntryItCreated(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/observed-newtag"}
	recordObserved(t, mem, memScope, "git_commit", "ba03f5969a", false)
	w := &pinningFake{}

	require.NoError(t, authz.PromoteObservedSlots(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{observedSlot("git_commit", "read")}, allowAll(), w, "alice", time.Now, 0))

	sc, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "observed", string(sc.Resources[0].Source))
}
