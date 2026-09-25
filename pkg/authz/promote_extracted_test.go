package authz_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	eex "github.com/authzed/openagentprimitives/pkg/memory/kinds/extracted_entity"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// recordCandidate writes what authzd's extractor would have recorded for a turn.
func recordCandidate(t *testing.T, mem memory.Memory, memScope memory.Scope, turn int, rt, id string) {
	t.Helper()
	require.NoError(t, eex.Record(systemCtx(), mem, memScope, eex.Content{
		ResourceType: rt, ResourceID: id, TurnIndex: turn,
		SourceText: "user said " + id, ExtractedAt: time.Unix(1700000000, 0).UTC(),
	}))
}

func querySlot(rt, perm string) authz.BoundEntitySpec {
	return authz.BoundEntitySpec{ResourceType: rt, Permission: perm, FillFrom: []string{"query"}}
}

func TestPromoteExtractedSlots_BindsCheckedCandidates(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/promote"}
	recordCandidate(t, mem, memScope, 3, "github_repo", "demo-org/demo-repo")
	w := &recordingRelWriter{}

	require.NoError(t, authz.PromoteExtractedSlots(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{querySlot("github_repo", "read")}, allowAll(), w, "alice", 3, time.Now, 0))

	require.Len(t, w.wrote, 1, "a Checked candidate must become a slot grant")
	assert.Equal(t, "github_repo", w.wrote[0].ResourceType)
	assert.Equal(t, "demo-org/demo-repo", w.wrote[0].ResourceID)
	assert.Equal(t, authz.SlotGrantRelationName("read"), w.wrote[0].Relation,
		"the relation carries the permission, so a read grant cannot satisfy write")

	// The Layer-2 half is not optional: a class with defaults already puts its
	// type into scope.Resources, so a grant without the matching ScopeResource
	// would be refused by the scope layer and the binding would do nothing.
	sc, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
	require.NoError(t, err)
	require.True(t, ok, "promotion must also narrow scope to the bound instance")
	assert.Equal(t, "extracted", string(sc.Resources[0].Source))
	assert.ElementsMatch(t, []string{"demo-org/demo-repo"}, sc.Resources[0].IDs)
}

// TestPromoteExtractedSlots_DropsWhatItMustNotBind covers the three filters,
// each of which closes a different hole. The extractor runs an LLM over the
// user's untrusted text in another process; everything it produces is a
// proposal, and each of these is a way a proposal could otherwise become
// authority nobody granted.
func TestPromoteExtractedSlots_DropsWhatItMustNotBind(t *testing.T) {
	cases := []struct {
		name      string
		candType  string
		candID    string
		declared  []authz.BoundEntitySpec
		chk       authz.Checker
		wantBound bool
	}{
		{
			name:     "type the class does not declare: dropped",
			candType: "crm_company", candID: "acme",
			declared:  []authz.BoundEntitySpec{querySlot("github_repo", "read")},
			chk:       allowAll(),
			wantBound: false,
		},
		{
			name:     "declared type whose fillFrom admits no extractor binding: dropped",
			candType: "github_repo", candID: "demo-org/demo-repo",
			// default-only: the class pins its own ids, so a value the
			// extractor found in a message is not one of them.
			declared:  []authz.BoundEntitySpec{{ResourceType: "github_repo", Permission: "read", FillFrom: []string{"default"}}},
			chk:       allowAll(),
			wantBound: false,
		},
		{
			name:     "requester has no standing: dropped, not escalated",
			candType: "github_repo", candID: "someone-else/private",
			declared: []authz.BoundEntitySpec{querySlot("github_repo", "read")},
			chk: authz.CheckerFunc(func(_ context.Context, _ authz.Permission, _ authz.Inputs) authz.Result {
				return authz.Result{Outcome: authz.OutcomeDenied, Message: "not yours"}
			}),
			wantBound: false,
		},
		{
			name:     "empty resource id names no instance: dropped",
			candType: "github_repo", candID: "",
			declared:  []authz.BoundEntitySpec{querySlot("github_repo", "read")},
			chk:       allowAll(),
			wantBound: false,
		},
		{
			name:     "declared, query-fillable and Checked: bound",
			candType: "github_repo", candID: "demo-org/demo-repo",
			declared:  []authz.BoundEntitySpec{querySlot("github_repo", "read")},
			chk:       allowAll(),
			wantBound: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			memScope := memory.Scope{Kind: "session", ID: "ns/" + tc.name}
			recordCandidate(t, mem, memScope, 1, tc.candType, tc.candID)
			w := &recordingRelWriter{}

			require.NoError(t, authz.PromoteExtractedSlots(systemCtx(), mem, memScope, bindSession(),
				tc.declared, tc.chk, w, "alice", 1, time.Now, 0))

			if tc.wantBound {
				assert.Len(t, w.wrote, 1)
				return
			}
			assert.Empty(t, w.wrote, "this candidate must not become a grant")
			_, ok, err := sessionscope.Get(systemCtx(), mem, memScope)
			require.NoError(t, err)
			assert.False(t, ok, "and must not widen the scope document either")
		})
	}
}

// TestPromoteExtractedSlots_OnlyThisTurn: candidates are recorded per turn, and
// promoting a turn must not sweep in another turn's proposals.
func TestPromoteExtractedSlots_OnlyThisTurn(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/promote-turns"}
	recordCandidate(t, mem, memScope, 1, "github_repo", "demo-org/turn-one")
	recordCandidate(t, mem, memScope, 2, "github_repo", "demo-org/turn-two")
	w := &recordingRelWriter{}

	require.NoError(t, authz.PromoteExtractedSlots(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{querySlot("github_repo", "read")}, allowAll(), w, "alice", 2, time.Now, 0))

	require.Len(t, w.wrote, 1)
	assert.Equal(t, "demo-org/turn-two", w.wrote[0].ResourceID)
}

func TestPromoteExtractedSlots_Idempotent(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/promote-idem"}
	recordCandidate(t, mem, memScope, 1, "github_repo", "demo-org/demo-repo")
	w := &recordingRelWriter{}
	specs := []authz.BoundEntitySpec{querySlot("github_repo", "read")}

	require.NoError(t, authz.PromoteExtractedSlots(systemCtx(), mem, memScope, bindSession(), specs, allowAll(), w, "alice", 1, time.Now, 0))
	require.NoError(t, authz.PromoteExtractedSlots(systemCtx(), mem, memScope, bindSession(), specs, allowAll(), w, "alice", 1, time.Now, 0))

	sc, _, _ := sessionscope.Get(systemCtx(), mem, memScope)
	require.Len(t, sc.Resources, 1)
	assert.ElementsMatch(t, []string{"demo-org/demo-repo"}, sc.Resources[0].IDs, "re-running a turn must not duplicate IDs")
}

func TestPromoteExtractedSlots_NoOpInputs(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/promote-noop"}
	recordCandidate(t, mem, memScope, 1, "github_repo", "demo-org/demo-repo")
	specs := []authz.BoundEntitySpec{querySlot("github_repo", "read")}

	cases := []struct {
		name    string
		specs   []authz.BoundEntitySpec
		chk     authz.Checker
		subject string
		turn    int
	}{
		{name: "nil checker", specs: specs, chk: nil, subject: "alice", turn: 1},
		{name: "empty subject", specs: specs, chk: allowAll(), subject: "", turn: 1},
		{name: "no declared slots", specs: nil, chk: allowAll(), subject: "alice", turn: 1},
		{name: "negative turn index", specs: specs, chk: allowAll(), subject: "alice", turn: -1},
		{name: "turn with no candidates", specs: specs, chk: allowAll(), subject: "alice", turn: 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &recordingRelWriter{}
			require.NoError(t, authz.PromoteExtractedSlots(systemCtx(), mem, memScope, bindSession(),
				tc.specs, tc.chk, w, tc.subject, tc.turn, time.Now, 0))
			assert.Empty(t, w.wrote)
		})
	}
}

// TestPromoteExtractedSlots_CandidateRunsThroughTheDeclaredTransformChain
// closes the same gap as the class-defaults version: the extractor's raw
// candidate must reach SpiceDB as the DERIVED id, since the dispatch-time
// Check computes the id via the same chain (ResolveResourceID →
// ApplyTransforms) and would never find a grant written under the raw value.
func TestPromoteExtractedSlots_CandidateRunsThroughTheDeclaredTransformChain(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/promote-transform"}
	recordCandidate(t, mem, memScope, 5, "git_repo", "https://github.com/acme/app")
	w := &recordingRelWriter{}
	slot := querySlot("git_repo", "push")
	slot.ValueTransforms = []string{"normalize_url", "spicedb_escape"}

	require.NoError(t, authz.PromoteExtractedSlots(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{slot}, allowAll(), w, "alice", 5, time.Now, 0))

	require.Len(t, w.wrote, 1)
	assert.Equal(t, "https=3A//github=2Ecom/acme/app", w.wrote[0].ResourceID,
		"the grant must carry the id the dispatch-time Check will compute, not the raw URL")
}

// An extracted candidate the declared chain cannot derive is dropped, the
// same way an undeclared type or a Check-denied candidate is dropped — never
// bound under its raw value.
func TestPromoteExtractedSlots_UndrivableCandidateIsDroppedNotBoundRaw(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/promote-badtransform"}
	recordCandidate(t, mem, memScope, 6, "git_repo", "https://github.com/acme/app")
	w := &recordingRelWriter{}
	slot := querySlot("git_repo", "push")
	slot.ValueTransforms = []string{"no_such_transform"}

	require.NoError(t, authz.PromoteExtractedSlots(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{slot}, allowAll(), w, "alice", 6, time.Now, 0))

	assert.Empty(t, w.wrote, "an undrivable candidate must bind nothing, not the raw value")
}

// A slot declaring only `ask` must bind what the user named in the reply. This
// is the whole substance of the ask source: the agent prompts using tools it
// already has, and the answer binds through this path.
func TestPromoteExtractedSlots_AskSlotBindsTheAnsweredValue(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/ask"}
	recordCandidate(t, mem, memScope, 4, "cluster", "cluster-7")
	w := &recordingRelWriter{}

	require.NoError(t, authz.PromoteExtractedSlots(systemCtx(), mem, memScope, bindSession(),
		[]authz.BoundEntitySpec{{ResourceType: "cluster", Permission: "debug", FillFrom: []string{"ask"}}},
		allowAll(), w, "alice", 4, time.Now, 0))

	require.Len(t, w.wrote, 1, "the value the user named in reply to the agent's question must bind")
	assert.Equal(t, "cluster", w.wrote[0].ResourceType)
	assert.Equal(t, "cluster-7", w.wrote[0].ResourceID)
}
