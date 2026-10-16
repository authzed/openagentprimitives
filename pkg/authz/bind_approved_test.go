package authz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

type conflictingScopeMemory struct {
	memory.Memory
	scope              memory.Scope
	conflictsRemaining int
	queryCalls         int
	putCalls           int
	putErr             error
}

// approvedRecordingWriter implements RelWriter AND SlotPinner: BindApproved
// binds single-occupancy slots, which the gate routes through the pinner, so a
// plain RelWriter would be refused. It records grants from either write path in
// `wrote` and keeps one pin per (scope,type).
//
// It also models the approved-MOVE path: `grantsFor` seeds a prior instance's
// held grants so ListGrantsFor returns them as the revoke set, `moves` records
// every MovePin call in order, and `ops` logs the operation sequence so a test
// can assert the move landed BEFORE the new instance's grant write.
type approvedRecordingWriter struct {
	wrote     []Relation
	pins      map[string]string
	grantsFor map[string][]Relation // (type\x00id) -> the instance's held grants, served by ListGrantsFor
	moves     []moveCall            // every MovePin, in order
	ops       []string              // ordered op log: "move", "grant"
	moveErr   error                 // injected into MovePin
	pinErr    map[string]error      // per resource type, injected into EnsurePin
	grantErr  error                 // injected into WriteGrantsPinned
	listErr   map[string]error      // per (type\x00id), injected into ListGrantsFor
}

type moveCall struct {
	resourceType, fromID, toID string
	revoke                     []Relation
}

func grantStoreKey(resourceType, resourceID string) string {
	return resourceType + "\x00" + resourceID
}

func (w *approvedRecordingWriter) WriteRelationships(_ context.Context, rels []Relation) error {
	w.ops = append(w.ops, "grant")
	w.wrote = append(w.wrote, rels...)
	return nil
}

func (*approvedRecordingWriter) DeleteRelationships(context.Context, []Relation) error { return nil }

func (w *approvedRecordingWriter) EnsurePin(_ context.Context, resourceType, resourceID string, scope SessionRef) (bool, string, error) {
	if err := w.pinErr[resourceType]; err != nil {
		return false, "", err
	}
	if w.pins == nil {
		w.pins = map[string]string{}
	}
	key := scope.String() + "\x00" + resourceType
	if cur, ok := w.pins[key]; ok {
		return true, cur, nil
	}
	w.pins[key] = resourceID
	return false, resourceID, nil
}

func (w *approvedRecordingWriter) WriteGrantsPinned(_ context.Context, rels []Relation, _, _ string, _ SessionRef) error {
	if w.grantErr != nil {
		return w.grantErr
	}
	w.ops = append(w.ops, "grant")
	w.wrote = append(w.wrote, rels...)
	return nil
}

func (w *approvedRecordingWriter) MovePin(_ context.Context, resourceType, fromID, toID string, revoke []Relation, scope SessionRef) error {
	if w.moveErr != nil {
		return w.moveErr
	}
	if w.pins == nil {
		w.pins = map[string]string{}
	}
	key := scope.String() + "\x00" + resourceType
	cur := w.pins[key]
	// Model the real MovePin's MUST_MATCH(fromID) and its idempotent no-op at the
	// target: if the pin already names toID, a prior phase of the same approval
	// landed this exact move, so it is a no-op (nil), never a drift. If the pin
	// is NEITHER fromID nor toID, MUST_MATCH fails for real — genuine drift.
	if cur == toID {
		return nil
	}
	if cur != fromID {
		return fmt.Errorf("fake move pin %s:%s -> %s:%s: %w", resourceType, fromID, resourceType, toID, ErrSlotPinned)
	}
	w.ops = append(w.ops, "move")
	w.moves = append(w.moves, moveCall{resourceType: resourceType, fromID: fromID, toID: toID, revoke: revoke})
	w.pins[key] = toID
	delete(w.grantsFor, grantStoreKey(resourceType, fromID))
	return nil
}

func (w *approvedRecordingWriter) ReadPin(_ context.Context, resourceType string, scope SessionRef) (string, error) {
	return w.pins[scope.String()+"\x00"+resourceType], nil
}

func (w *approvedRecordingWriter) ListGrantsFor(_ context.Context, resourceType, resourceID string, _ SessionRef) ([]Relation, error) {
	if err := w.listErr[grantStoreKey(resourceType, resourceID)]; err != nil {
		return nil, err
	}
	return w.grantsFor[grantStoreKey(resourceType, resourceID)], nil
}

func (m *conflictingScopeMemory) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	m.queryCalls++
	// bindSlots reads once to compute its delta, then PutIfVersion reads again
	// to compare. Change the document immediately before that comparison so the
	// caller observes the same optimistic-concurrency loss as two live writers.
	if m.conflictsRemaining > 0 && m.queryCalls%2 == 0 {
		cur, found, err := sessionscope.Get(ctx, m.Memory, m.scope)
		if err != nil {
			return memory.QueryResult{}, err
		}
		if !found {
			cur = scope.Scope{}
		}
		cur = scope.ApplyDelta(cur, scope.ScopeDelta{
			HardDeny: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "git_repo", ID: "blocked/repo"}}},
		}, scope.SourceApproverDenyConv, time.Unix(1700000000, 0).UTC())
		if err := sessionscope.Put(ctx, m.Memory, m.scope, cur); err != nil {
			return memory.QueryResult{}, err
		}
		m.conflictsRemaining--
	}
	return m.Memory.Query(ctx, q)
}

func (m *conflictingScopeMemory) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	m.putCalls++
	if m.putErr != nil {
		return memory.Entry{}, m.putErr
	}
	return m.Memory.Put(ctx, e)
}

// A human approving a slot must NARROW the session, not merely permit one more
// instance. Approving acme/app excludes every other git_repo — the same
// property a class `default` has had all along, and the reason the weaker
// config path was deterministic while the stronger human one was not.
func TestBindApproved_narrowsScopeToTheApprovedInstance(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	sess := SessionRef{Namespace: "ns", Name: "s"}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	err := BindApproved(ctx, mem, memScope, sess, nil,
		[]SlotBinding{{ResourceType: "git_repo", ResourceID: TrustedObjectID("acme/app"), Permission: "push"}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now)
	require.NoError(t, err, "a nil writer narrows scope and grants nothing; it must not error")

	got, _, err := sessionscope.Get(memory.WithSystemApproval(ctx, "test"), mem, memScope)
	require.NoError(t, err)

	var ids []string
	for _, r := range got.Resources {
		if r.ResourceType == "git_repo" {
			ids = append(ids, r.IDs...)
		}
	}
	assert.Equal(t, []string{"acme/app"}, ids,
		"once a type is in scope.Resources every ref of that type must match one, "+
			"so this is what makes a second repository denied rather than merely ungranted")
}

// The source is recorded, and it is its own value. Recording an approval as
// `default` or `extracted` would misattribute a human decision to config or to
// text the agent read — the audit trail is the only place that distinction
// survives.
func TestBindApproved_recordsApprovedAsItsOwnSource(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	require.NoError(t, BindApproved(ctx, mem, memScope,
		SessionRef{Namespace: "ns", Name: "s"}, nil,
		[]SlotBinding{{ResourceType: "git_repo", ResourceID: TrustedObjectID("acme/app"), Permission: "push"}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now))

	got, _, err := sessionscope.Get(memory.WithSystemApproval(ctx, "test"), mem, memScope)
	require.NoError(t, err)
	require.NotEmpty(t, got.Resources)
	assert.Equal(t, scope.SourceApproved, got.Resources[0].Source)
}

// Empty is a no-op, not an error: a decision that granted nothing (every slot
// held back for its owner) must not fail the turn.
func TestBindApproved_emptyBindingsIsANoOp(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	assert.NoError(t, BindApproved(context.Background(), mem, memory.Scope{Kind: "session", ID: "ns/s"},
		SessionRef{Namespace: "ns", Name: "s"}, nil, nil,
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now))
}

func TestBindApproved_retriesScopeVersionConflictAndMergesConcurrentDeny(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	mem := &conflictingScopeMemory{
		Memory:             memory.NewLocal(inmem.NewBackend()),
		scope:              memScope,
		conflictsRemaining: 1,
	}
	w := &approvedRecordingWriter{}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	require.NoError(t, BindApproved(ctx, mem, memScope,
		SessionRef{Namespace: "ns", Name: "s"}, w,
		[]SlotBinding{{ResourceType: "git_repo", ResourceID: TrustedObjectID("acme/app"), Permission: "push"}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now))

	got, _, err := sessionscope.Get(ctx, mem.Memory, memScope)
	require.NoError(t, err)
	assert.True(t, got.ResourceDisallowed("git_repo", "blocked/repo"),
		"the retry must re-read and preserve the concurrent hard deny")
	assert.Equal(t, 4, mem.queryCalls, "one lost compare requires exactly one re-read/recompute")
	assert.Len(t, w.wrote, 1, "grant only after the merged scope write succeeds")
}

func TestBindApproved_exhaustsScopeVersionConflictsWithoutGranting(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	mem := &conflictingScopeMemory{
		Memory:             memory.NewLocal(inmem.NewBackend()),
		scope:              memScope,
		conflictsRemaining: 100,
	}
	w := &approvedRecordingWriter{}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	err := BindApproved(ctx, mem, memScope,
		SessionRef{Namespace: "ns", Name: "s"}, w,
		[]SlotBinding{{ResourceType: "git_repo", ResourceID: TrustedObjectID("acme/app"), Permission: "push"}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now)

	assert.ErrorIs(t, err, sessionscope.ErrVersionConflict)
	assert.Equal(t, 6, mem.queryCalls, "the retry bound is three read/compare attempts")
	assert.Equal(t, 97, mem.conflictsRemaining)
	assert.Empty(t, w.wrote, "a scope write that never wins must never create a grant")
}

// The approved MOVE — the only way a filled single-occupancy slot legitimately
// changes instance. A binding carrying PriorID repoints the pin repoA -> repoB,
// revokes repoA's held grants in the SAME request, and only then writes repoB's
// grant through the normal gated path. Order matters: the revoke-and-move lands
// before the new grant, so the slot is never momentarily held by two instances.
func TestBindApproved_approvedMoveRepointsPinRevokesPriorGrantsThenWritesNew(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	sess := SessionRef{Namespace: "ns", Name: "s"}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	priorGrant := SlotGrantRelation("git_repo", "repoA", "push", sess)
	w := &approvedRecordingWriter{
		pins:      map[string]string{sess.String() + "\x00git_repo": "repoA"},
		grantsFor: map[string][]Relation{grantStoreKey("git_repo", "repoA"): {priorGrant}},
	}

	err := BindApproved(ctx, mem, memScope, sess, w,
		[]SlotBinding{{
			ResourceType: "git_repo", ResourceID: TrustedObjectID("repoB"),
			Permission: "push", PriorID: TrustedObjectID("repoA"),
		}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now)
	require.NoError(t, err)

	// Exactly one move, repointing repoA -> repoB, revoking repoA's held grant.
	require.Len(t, w.moves, 1, "one move per moving type")
	assert.Equal(t, "git_repo", w.moves[0].resourceType)
	assert.Equal(t, "repoA", w.moves[0].fromID)
	assert.Equal(t, "repoB", w.moves[0].toID)
	require.Len(t, w.moves[0].revoke, 1, "the revoke set is the prior instance's actual grants")
	assert.Equal(t, priorGrant, w.moves[0].revoke[0])

	// The move lands BEFORE the new instance's grant write.
	require.GreaterOrEqual(t, len(w.ops), 2)
	assert.Equal(t, "move", w.ops[0], "the move-and-revoke happens first")
	assert.Equal(t, "grant", w.ops[len(w.ops)-1], "the new grant is written after the move")

	// repoB is now pinned and holds the new grant.
	assert.Equal(t, "repoB", w.pins[sess.String()+"\x00git_repo"])
	newGrant := SlotGrantRelation("git_repo", "repoB", "push", sess)
	assert.True(t, slices.ContainsFunc(w.wrote, func(r Relation) bool {
		return r.ResourceType == newGrant.ResourceType && r.ResourceID == newGrant.ResourceID && r.Relation == newGrant.Relation
	}), "the new instance's grant must be written after the move")
}

// A move whose MUST_MATCH no longer holds — the pin drifted since the card was
// shown — fails. BindApproved must PROPAGATE that failure (errors.Is-able as
// ErrSlotPinned), never swallow it: the human's approval produced nothing, and
// nothing may be written for the new instance either.
func TestBindApproved_moveFailurePropagatesWrappingErrSlotPinned(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	sess := SessionRef{Namespace: "ns", Name: "s"}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	w := &approvedRecordingWriter{
		pins:    map[string]string{sess.String() + "\x00git_repo": "repoA"},
		moveErr: fmt.Errorf("move pin git_repo:repoA -> git_repo:repoB: %w", ErrSlotPinned),
	}

	err := BindApproved(ctx, mem, memScope, sess, w,
		[]SlotBinding{{
			ResourceType: "git_repo", ResourceID: TrustedObjectID("repoB"),
			Permission: "push", PriorID: TrustedObjectID("repoA"),
		}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSlotPinned, "a failed move must propagate, not be swallowed")
	assert.ErrorIs(t, err, ErrSlotMoveDrifted, "a MUST_MATCH failure is a drifted move, which the approver may retry")
	assert.NotErrorIs(t, err, ErrSlotMoveCommitted, "nothing moved, so nothing was committed")
	assert.Empty(t, w.wrote, "a move that failed must not go on to write the new instance's grant")
}

// The move refusals that must happen BEFORE anything is repointed: each is a
// deterministic answer that a MovePin followed by a failed grant would turn
// into "the old instance is revoked and the new one is not granted".
func TestBindApproved_moveRefusedBeforeAnyMove(t *testing.T) {
	sess := SessionRef{Namespace: "ns", Name: "s"}
	cases := []struct {
		name     string
		bindings []SlotBinding
	}{
		{
			name: "rebind never: refused with the new-session route, pin untouched",
			bindings: []SlotBinding{{
				ResourceType: "git_repo", ResourceID: TrustedObjectID("repoB"), Permission: "push",
				PriorID: TrustedObjectID("repoA"), Rebind: SlotRebindNever,
			}},
		},
		{
			name: "same approval also binds a constant instance of the moving type: refused, pin untouched",
			bindings: []SlotBinding{
				{ResourceType: "git_repo", ResourceID: TrustedObjectID("repoB"), Permission: "push", PriorID: TrustedObjectID("repoA")},
				{ResourceType: "git_repo", ResourceID: TrustedObjectID("workspace"), Permission: "read"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := memory.WithSystemApproval(context.Background(), "test")
			mem := memory.NewLocal(inmem.NewBackend())
			now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
			priorGrant := SlotGrantRelation("git_repo", "repoA", "push", sess)
			w := &approvedRecordingWriter{
				pins:      map[string]string{sess.String() + "\x00git_repo": "repoA"},
				grantsFor: map[string][]Relation{grantStoreKey("git_repo", "repoA"): {priorGrant}},
			}

			err := BindApproved(ctx, mem, memory.Scope{Kind: "session", ID: "ns/s"}, sess, w,
				tc.bindings, EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now)

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrSlotPinned)
			assert.NotErrorIs(t, err, ErrSlotMoveDrifted, "a deterministic refusal is not a retryable drift")
			assert.Empty(t, w.moves, "no MovePin may run")
			assert.Equal(t, "repoA", w.pins[sess.String()+"\x00git_repo"], "the pin stays on the prior instance")
			assert.NotEmpty(t, w.grantsFor[grantStoreKey("git_repo", "repoA")], "the prior instance keeps its grants")
		})
	}
}

// A grant failure AFTER the move landed must say so: the displaced instance's
// grants are already revoked, so an error that reads as "nothing happened"
// would have the approver told nothing was granted while access was removed.
func TestBindApproved_grantFailureAfterMoveReportsCommittedMove(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	sess := SessionRef{Namespace: "ns", Name: "s"}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	w := &approvedRecordingWriter{
		pins:     map[string]string{sess.String() + "\x00git_repo": "repoA"},
		grantErr: errors.New("spicedb unavailable"),
	}

	err := BindApproved(ctx, mem, memory.Scope{Kind: "session", ID: "ns/s"}, sess, w,
		[]SlotBinding{{
			ResourceType: "git_repo", ResourceID: TrustedObjectID("repoB"),
			Permission: "push", PriorID: TrustedObjectID("repoA"),
		}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSlotMoveCommitted)
	assert.Len(t, w.moves, 1, "the move itself landed")
	assert.Equal(t, "repoB", w.pins[sess.String()+"\x00git_repo"])
}

// One type's store failure must NOT strand another type's move-and-grant.
// GrantSlots handles each type independently, so when crm_company's revoke-set
// read fails, git_repo's move has ALREADY landed its new grant in git_repo's own
// iteration — not been abandoned before bindSlots ran (the all-moves-first pass
// this replaces would have dropped it). crm_company's failure rides out as a
// transient store fault for that type alone, never mislabeled as a committed
// move of git_repo.
func TestBindApproved_oneTypesStoreFailureLeavesAnotherTypesMoveGrantIntact(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	sess := SessionRef{Namespace: "ns", Name: "s"}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	gitPrior := SlotGrantRelation("git_repo", "repoA", "push", sess)
	w := &approvedRecordingWriter{
		pins: map[string]string{
			sess.String() + "\x00git_repo":    "repoA",
			sess.String() + "\x00crm_company": "coX",
		},
		grantsFor: map[string][]Relation{grantStoreKey("git_repo", "repoA"): {gitPrior}},
		// crm_company's revoke-set read fails; git_repo's move is unaffected.
		listErr: map[string]error{grantStoreKey("crm_company", "coX"): errors.New("spicedb read unavailable")},
	}

	err := BindApproved(ctx, mem, memory.Scope{Kind: "session", ID: "ns/s"}, sess, w,
		[]SlotBinding{
			{ResourceType: "git_repo", ResourceID: TrustedObjectID("repoB"), Permission: "push", PriorID: TrustedObjectID("repoA")},
			{ResourceType: "crm_company", ResourceID: TrustedObjectID("coY"), Permission: "contact_access", PriorID: TrustedObjectID("coX")},
		},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSlotStore, "the failing type surfaces as a transient store fault")
	assert.NotErrorIs(t, err, ErrSlotMoveCommitted,
		"git_repo's success must not be dragged into a committed-move report for a sibling's failure")
	// git_repo fully landed: moved and granted.
	assert.Len(t, w.moves, 1, "git_repo's move landed independently")
	assert.Equal(t, "repoB", w.pins[sess.String()+"\x00git_repo"], "git_repo repointed to the new instance")
	newGrant := SlotGrantRelation("git_repo", "repoB", "push", sess)
	assert.True(t, slices.ContainsFunc(w.wrote, func(r Relation) bool {
		return r.ResourceType == newGrant.ResourceType && r.ResourceID == newGrant.ResourceID && r.Relation == newGrant.Relation
	}), "git_repo's new grant is written, not stranded by the sibling type's failure")
	// crm_company never moved.
	assert.Equal(t, "coX", w.pins[sess.String()+"\x00crm_company"], "the failing type's pin is untouched")
}

// A plan-scoped approval resolves one BindApproved call per covered phase, and
// each re-attempts the SAME move. The first repoints the pin; every later one
// finds the pin already at the target and is a no-op (MovePin's idempotency) —
// not a drift that drops that phase's grant. This is what lets the move stop
// depending on which covered phase happens to run first.
func TestBindApproved_repeatedMoveAcrossPhasesIsIdempotentNotDrift(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	sess := SessionRef{Namespace: "ns", Name: "s"}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	gitPrior := SlotGrantRelation("git_repo", "repoA", "push", sess)
	w := &approvedRecordingWriter{
		pins:      map[string]string{sess.String() + "\x00git_repo": "repoA"},
		grantsFor: map[string][]Relation{grantStoreKey("git_repo", "repoA"): {gitPrior}},
	}

	move := []SlotBinding{{
		ResourceType: "git_repo", ResourceID: TrustedObjectID("repoB"),
		Permission: "push", PriorID: TrustedObjectID("repoA"),
	}}

	// Phase 1: the real move.
	require.NoError(t, BindApproved(ctx, mem, memScope, sess, w, move,
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now))
	assert.Len(t, w.moves, 1, "the first phase performs the real move")
	assert.Equal(t, "repoB", w.pins[sess.String()+"\x00git_repo"])

	// Phase 2: the SAME move, with the now-stale PriorID=repoA. Must succeed as a
	// no-op (pin already repoB) and still write the grant — not drift-fail.
	require.NoError(t, BindApproved(ctx, mem, memScope, sess, w, move,
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now),
		"a repeated move whose target is already pinned must be an idempotent no-op, not a drift")
	assert.Len(t, w.moves, 1, "no second MovePin is performed; the pin already names the target")
	assert.Equal(t, "repoB", w.pins[sess.String()+"\x00git_repo"], "the pin stays on the target")
}

// Two DISTINCT move targets for ONE single-occupancy type in one approval
// cannot both occupy the slot. GrantSlots refuses the type (two distinct
// instances) BEFORE any MovePin runs — executing even the first would commit the
// slot to whichever target happened to be ordered first, an instance the
// approver was not necessarily shown as THE move.
func TestBindApproved_twoDistinctMoveTargetsRefusedBeforeAnyMove(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	sess := SessionRef{Namespace: "ns", Name: "s"}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	w := &approvedRecordingWriter{pins: map[string]string{sess.String() + "\x00git_repo": "repoA"}}

	err := BindApproved(ctx, mem, memScope, sess, w,
		[]SlotBinding{
			{ResourceType: "git_repo", ResourceID: TrustedObjectID("repoB"), Permission: "push", PriorID: TrustedObjectID("repoA")},
			{ResourceType: "git_repo", ResourceID: TrustedObjectID("repoC"), Permission: "push", PriorID: TrustedObjectID("repoA")},
		},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSlotPinned, "two distinct move targets is a single-occupancy refusal")
	assert.Empty(t, w.moves, "no MovePin may run when the move set is self-contradictory")
	assert.Empty(t, w.wrote, "a refused move set grants nothing")
}

// A binding whose PriorID equals its ResourceID is not a move: the pin already
// names that instance. GrantSlots must SKIP the move (no MovePin, no needless
// revoke of the instance's own grants) and bind it through the normal
// same-instance pinned path (EnsurePin + WriteGrantsPinned).
func TestBindApproved_moveToSameInstanceIsSkippedNotMoved(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	sess := SessionRef{Namespace: "ns", Name: "s"}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	w := &approvedRecordingWriter{pins: map[string]string{sess.String() + "\x00git_repo": "repoA"}}

	err := BindApproved(ctx, mem, memScope, sess, w,
		[]SlotBinding{{
			ResourceType: "git_repo", ResourceID: TrustedObjectID("repoA"),
			Permission: "push", PriorID: TrustedObjectID("repoA"),
		}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now)

	require.NoError(t, err)
	assert.Empty(t, w.moves, "a move to the SAME instance is a no-op, never a MovePin")
	assert.Equal(t, "repoA", w.pins[sess.String()+"\x00git_repo"], "the pin stays put")
	assert.NotEmpty(t, w.wrote, "the same-instance re-grant still binds through the normal pinned path")
}

func TestBindApproved_doesNotRetryUnrelatedScopeWriteError(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	errStore := errors.New("memory unavailable")
	mem := &conflictingScopeMemory{
		Memory: memory.NewLocal(inmem.NewBackend()),
		scope:  memScope,
		putErr: errStore,
	}
	w := &approvedRecordingWriter{}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	err := BindApproved(ctx, mem, memScope,
		SessionRef{Namespace: "ns", Name: "s"}, w,
		[]SlotBinding{{ResourceType: "git_repo", ResourceID: TrustedObjectID("acme/app"), Permission: "push"}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now)

	assert.ErrorIs(t, err, errStore)
	assert.Equal(t, 1, mem.putCalls, "only version conflicts are retryable")
	assert.Empty(t, w.wrote, "a failed scope write must never create a grant")
}
