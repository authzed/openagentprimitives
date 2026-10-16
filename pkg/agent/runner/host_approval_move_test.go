package runner

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// moveTrackingBinder is a SlotBinder for the approved-move path. `pinnedTo`
// seeds the current pin ("" = unpinned, a clean first-fill). MovePin always
// fails as ErrSlotPinned (the pin drifted since the card) and counts its calls,
// so a test can both check the failure surfaces AND prove a first-fill never
// attempts a move at all.
type moveTrackingBinder struct {
	pinnedTo  string
	moveCalls int
}

func (*moveTrackingBinder) WriteRelationships(context.Context, []authz.Relation) error  { return nil }
func (*moveTrackingBinder) DeleteRelationships(context.Context, []authz.Relation) error { return nil }
func (b *moveTrackingBinder) EnsurePin(_ context.Context, _, resourceID string, _ authz.SessionRef) (bool, string, error) {
	if b.pinnedTo == "" {
		// First-in-wins: this call writes the pin, nothing was held before.
		return false, resourceID, nil
	}
	return true, b.pinnedTo, nil
}
func (*moveTrackingBinder) WriteGrantsPinned(context.Context, []authz.Relation, string, string, authz.SessionRef) error {
	return nil
}
func (b *moveTrackingBinder) MovePin(_ context.Context, _, fromID, toID string, _ []authz.Relation, _ authz.SessionRef) error {
	b.moveCalls++
	return fmt.Errorf("move pin git_repo:%s -> git_repo:%s: %w", fromID, toID, authz.ErrSlotPinned)
}
func (b *moveTrackingBinder) ReadPin(context.Context, string, authz.SessionRef) (string, error) {
	return b.pinnedTo, nil
}
func (*moveTrackingBinder) ListGrantsFor(context.Context, string, string, authz.SessionRef) ([]authz.Relation, error) {
	return nil, nil
}

var _ authz.SlotPinner = (*moveTrackingBinder)(nil)

// movingBinder is a STATEFUL SlotPinner for the multi-phase approved-move path.
// The pin starts at pinned[type]; a MovePin whose fromID matches the live pin
// SUCCEEDS and repoints it, while one whose fromID no longer matches fails as a
// drift (ErrSlotPinned) — exactly as SpiceDB's MUST_MATCH precondition would. It
// records every grant written (WriteGrantsPinned) so a test can assert which
// (type, id, permission) grants actually landed.
type movingBinder struct {
	pinned    map[string]string // resourceType -> currently pinned instance
	moveCalls int
	wrote     []authz.Relation
}

func (*movingBinder) WriteRelationships(context.Context, []authz.Relation) error  { return nil }
func (*movingBinder) DeleteRelationships(context.Context, []authz.Relation) error { return nil }

func (b *movingBinder) EnsurePin(_ context.Context, resourceType, resourceID string, _ authz.SessionRef) (bool, string, error) {
	if cur := b.pinned[resourceType]; cur != "" {
		return true, cur, nil
	}
	b.pinned[resourceType] = resourceID
	return false, resourceID, nil
}

func (b *movingBinder) WriteGrantsPinned(_ context.Context, rels []authz.Relation, _, _ string, _ authz.SessionRef) error {
	b.wrote = append(b.wrote, rels...)
	return nil
}

func (b *movingBinder) MovePin(_ context.Context, resourceType, fromID, toID string, _ []authz.Relation, _ authz.SessionRef) error {
	cur := b.pinned[resourceType]
	if cur == toID {
		// Idempotent no-op: the pin already names the target (an earlier phase of
		// this same approval moved it). Matches the real MovePin contract — no
		// repoint, so moveCalls is NOT incremented.
		return nil
	}
	if cur != fromID {
		// Genuine drift: the pin is neither the displaced instance nor the target.
		return fmt.Errorf("move pin %s:%s -> %s:%s: %w", resourceType, fromID, resourceType, toID, authz.ErrSlotPinned)
	}
	// A real repoint. moveCalls counts these, so "moves at most once per approval"
	// is observable even though MovePin is INVOKED once per covered phase.
	b.moveCalls++
	b.pinned[resourceType] = toID
	return nil
}

func (b *movingBinder) ReadPin(_ context.Context, resourceType string, _ authz.SessionRef) (string, error) {
	return b.pinned[resourceType], nil
}

func (*movingBinder) ListGrantsFor(context.Context, string, string, authz.SessionRef) ([]authz.Relation, error) {
	return nil, nil
}

func (b *movingBinder) wroteGrant(resourceType, resourceID, permission string) bool {
	want := authz.SlotGrantRelationName(permission)
	for _, r := range b.wrote {
		if r.ResourceType == resourceType && r.ResourceID == resourceID && r.Relation == want {
			return true
		}
	}
	return false
}

var _ authz.SlotPinner = (*movingBinder)(nil)

// A move the human approved that cannot execute — the pin drifted since the
// card — is a BROKEN PROMISE, not an operator-log footnote. narrowToApproved
// must surface it as a failed approval (the InternalError notice that tells the
// approver to try again and that nothing was granted), the same mechanism a
// discarded-approval uses, rather than swallowing the ErrSlotPinned in a
// slog.Info the approver never sees.
func TestNarrowToApproved_approvedMoveFailureTellsTheApprover(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	var published []channelevents.Envelope
	h := planGateHost(t, &channelevents.Envelope{})
	h.l.Mem = mem
	binder := &moveTrackingBinder{pinnedTo: "repoA"}
	h.l.SlotBinder = binder
	h.l.PlanGateSlotPermissionSets = map[string][]string{"git_repo": {"push"}}
	h.l.InteractionRequestPublish = func(_ context.Context, _, _ string, env channelevents.Envelope) error {
		published = append(published, env)
		return nil
	}

	pushHandle, err := permsurface.NewPermHandle("push", "git_repo")
	require.NoError(t, err)
	idx := int32(0)
	ask := planPhaseAsk()
	ask.Payload["phaseKey"] = "p1"
	ask.Payload["ceiling"] = []string{pushHandle.String()}
	// The card recorded a MOVE: the session is pinned to repoA and the phase
	// names repoB. A yes must repoint the pin — and when it cannot, say so.
	ask.Payload["covered"] = []plangateaudit.Content{{
		Event:      plangateaudit.EventPhaseApproved,
		PhaseKey:   "p1",
		PhaseIndex: &idx,
		Ceiling:    []string{pushHandle.String()},
		Slots:      []string{"git_repo"},
		SlotRefs:   []plangateaudit.SlotRef{{Type: "git_repo", ID: "repoB", MovedFrom: "repoA"}},
	}}

	driveDecision(t, h, mem, ask, true)

	assert.Equal(t, 1, binder.moveCalls, "the approved move was attempted")
	var told bool
	for _, env := range published {
		pl := decodeInteractionRequest(t, env)
		if pl.Category == categories.InternalError {
			told = true
			// A DRIFTED MOVE is a recoverable fault: the "fault on our side, try
			// once more" copy, not the committed-elsewhere refusal copy.
			assert.Contains(t, pl.Lead, "fault on our side",
				"a drifted move is a fault, not a refusal")
			assert.Contains(t, pl.NextStep, "Try approving once more")
			assert.NotContains(t, pl.Lead, "already committed",
				"a drifted move must not read as a plain pinned refusal")
		}
	}
	assert.True(t, told,
		"an approved move that could not execute must tell the approver, not vanish into a log line")
}

// A plain pinned refusal — the plan named a different instance of a slot already
// committed to another, with NO move approved (no MovedFrom) — is an answer, not
// a fault. narrowToApproved must tell the approver the slot is committed
// elsewhere, that other instances may still have bound, and that re-approving
// this card will not move it — the opposite advice to the drifted-move case.
func TestNarrowToApproved_plainPinnedRefusalSaysCommittedElsewhere(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	var published []channelevents.Envelope
	h := planGateHost(t, &channelevents.Envelope{})
	h.l.Mem = mem
	// Pinned to repoA; the phase names repoB with NO MovedFrom, so no move is
	// approved. GrantSlots refuses the different-instance bind as ErrSlotPinned.
	binder := &moveTrackingBinder{pinnedTo: "repoA"}
	h.l.SlotBinder = binder
	h.l.PlanGateSlotPermissionSets = map[string][]string{"git_repo": {"push"}}
	h.l.InteractionRequestPublish = func(_ context.Context, _, _ string, env channelevents.Envelope) error {
		published = append(published, env)
		return nil
	}

	pushHandle, err := permsurface.NewPermHandle("push", "git_repo")
	require.NoError(t, err)
	idx := int32(0)
	ask := planPhaseAsk()
	ask.Payload["phaseKey"] = "p1"
	ask.Payload["ceiling"] = []string{pushHandle.String()}
	ask.Payload["covered"] = []plangateaudit.Content{{
		Event:      plangateaudit.EventPhaseApproved,
		PhaseKey:   "p1",
		PhaseIndex: &idx,
		Ceiling:    []string{pushHandle.String()},
		Slots:      []string{"git_repo"},
		// No MovedFrom: the bind is refused outright, not moved.
		SlotRefs: []plangateaudit.SlotRef{{Type: "git_repo", ID: "repoB"}},
	}}

	driveDecision(t, h, mem, ask, true)

	assert.Zero(t, binder.moveCalls, "a plain refusal names no prior pin, so it never attempts a move")
	var told bool
	for _, env := range published {
		pl := decodeInteractionRequest(t, env)
		assert.NotEqual(t, categories.InternalError, pl.Category,
			"a plain pinned refusal is an answer, not a fault — it must not carry the internal-error (we-broke, retry) category")
		if pl.Category == categories.SlotBindRefused {
			told = true
			assert.Contains(t, pl.Lead, "already committed to a different target",
				"a plain pinned refusal names the commitment, not a fault")
			assert.Contains(t, pl.Lead, "Other instances",
				"it must not claim nothing was granted — other types may have bound")
			assert.Contains(t, pl.NextStep, "Re-approving this card will not move it")
			// A movable (default-rebind) slot: the route is an amendment naming the
			// instance the approval WANTED, then a new session — never pointed at
			// the already-committed instance the old copy named.
			assert.Contains(t, pl.NextStep, "the instance you wanted",
				"the route must name the instance the approval wanted, not the committed one")
			assert.Contains(t, pl.NextStep, "approved amendment moves the commitment",
				"a movable slot's commitment can be moved by an approved amendment")
			assert.NotContains(t, pl.NextStep, "To target the committed instance",
				"the old copy pointed the approver at the wrong instance")
			assert.NotContains(t, pl.Lead, "fault on our side",
				"a refusal is not a fault")
		}
	}
	assert.True(t, told, "a plain pinned refusal must tell the approver (under the slot-bind-refused category) why their approval did not bind this instance")
}

// A pinned refusal whose every refused slot is `rebind: never` must route to a
// NEW SESSION only: no amendment can move a commitment fixed for the session's
// life, so the amendment copy would lead the approver into widenings refused
// every time. The notice says the commitment cannot be moved and to start a new
// session — never the amendment route.
func TestPublishPlanGateApprovalFailed_rebindNeverRoutesToNewSessionOnly(t *testing.T) {
	var published []channelevents.Envelope
	h := planGateHost(t, &channelevents.Envelope{})
	h.l.InteractionRequestPublish = func(_ context.Context, _, _ string, env channelevents.Envelope) error {
		published = append(published, env)
		return nil
	}
	cause := fmt.Errorf("pinned to git_repo:repoA: %w", authz.ErrSlotPinned)

	h.publishPlanGateApprovalFailed(context.Background(), cause, approvalPinnedRefusal, true)

	var told bool
	for _, env := range published {
		pl := decodeInteractionRequest(t, env)
		if pl.Category != categories.SlotBindRefused {
			continue
		}
		told = true
		assert.Contains(t, pl.NextStep, "cannot be moved", "a never slot's commitment cannot be moved")
		assert.Contains(t, pl.NextStep, "Start a new session", "the only route is a new session")
		assert.NotContains(t, pl.NextStep, "amendment",
			"a never slot must not be pointed at an amendment that can never succeed")
	}
	assert.True(t, told, "a never pinned refusal must still tell the approver, under the slot-bind-refused category")
}

// The control: the SAME binding WITHOUT a recorded move (a first-fill) does not
// trip the move path at all — ErrSlotPinned is never produced, so no
// InternalError notice is raised. This pins that the re-raise is specific to a
// failed MOVE, not to any slot approval.
func TestNarrowToApproved_firstFillDoesNotRaiseAMoveFailure(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	var published []channelevents.Envelope
	h := planGateHost(t, &channelevents.Envelope{})
	h.l.Mem = mem
	// Unpinned: a genuine first-fill. MovePin would fail IF reached, so moveCalls
	// staying 0 proves the move path was never entered.
	binder := &moveTrackingBinder{pinnedTo: ""}
	h.l.SlotBinder = binder
	h.l.PlanGateSlotPermissionSets = map[string][]string{"git_repo": {"push"}}
	h.l.InteractionRequestPublish = func(_ context.Context, _, _ string, env channelevents.Envelope) error {
		published = append(published, env)
		return nil
	}

	pushHandle, err := permsurface.NewPermHandle("push", "git_repo")
	require.NoError(t, err)
	idx := int32(0)
	ask := planPhaseAsk()
	ask.Payload["phaseKey"] = "p1"
	ask.Payload["ceiling"] = []string{pushHandle.String()}
	ask.Payload["covered"] = []plangateaudit.Content{{
		Event:      plangateaudit.EventPhaseApproved,
		PhaseKey:   "p1",
		PhaseIndex: &idx,
		Ceiling:    []string{pushHandle.String()},
		Slots:      []string{"git_repo"},
		// No MovedFrom: a first-fill. The move path must not run.
		SlotRefs: []plangateaudit.SlotRef{{Type: "git_repo", ID: "repoB"}},
	}}

	driveDecision(t, h, mem, ask, true)

	assert.Zero(t, binder.moveCalls, "a first-fill must never attempt a move")
	for _, env := range published {
		assert.NotEqual(t, categories.InternalError, decodeInteractionRequest(t, env).Category,
			"a first-fill names no prior pin, so it never attempts a move and never fails one")
	}
}

// A plan-scoped approval that covers TWO phases both naming the moved-to
// instance of ONE single-occupancy slot must repoint the pin exactly ONCE and
// bind BOTH phases' permissions. stampMovedFrom stamps the same repoA->repoB
// move onto every covered record, and the approval is resolved one BindApproved
// call PER covered record — so the first phase repoints repoA->repoB and the
// second re-attempts the identical move with a now-stale PriorID=repoA. Because
// MovePin is idempotent at the target (the pin already names repoB), the second
// attempt is a no-op rather than a drift-fail: both phases' grants land, no
// phase is wedged, and every covered record keeps its MovedFrom for the audit
// trail. This is what replaced the earlier dedupe-the-move band-aid.
func TestRecordPlanGateDecision_multiPhaseMoveBindsEveryCoveredPhase(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	var published []channelevents.Envelope
	h := planGateHost(t, &channelevents.Envelope{})
	h.l.Mem = mem
	binder := &movingBinder{pinned: map[string]string{"git_repo": "repoA"}}
	h.l.SlotBinder = binder
	h.l.PlanGateSlotPermissionSets = map[string][]string{"git_repo": {"read", "push"}}
	h.l.InteractionRequestPublish = func(_ context.Context, _, _ string, env channelevents.Envelope) error {
		published = append(published, env)
		return nil
	}

	readHandle, err := permsurface.NewPermHandle("read", "git_repo")
	require.NoError(t, err)
	pushHandle, err := permsurface.NewPermHandle("push", "git_repo")
	require.NoError(t, err)
	idx0, idx2 := int32(0), int32(2)
	ask := planPhaseAsk()
	ask.Payload["phaseKey"] = "p0"
	ask.Payload["ceiling"] = []string{readHandle.String()}
	// Session pinned to repoA; both covered phases name repoB. The read phase
	// (0) and the push phase (2) each carry the same stamped repoA->repoB move.
	ask.Payload["covered"] = []plangateaudit.Content{
		{
			Event: plangateaudit.EventPhaseApproved, PhaseKey: "p0", PhaseIndex: &idx0,
			Ceiling: []string{readHandle.String()}, Slots: []string{"git_repo"},
			SlotRefs: []plangateaudit.SlotRef{{Type: "git_repo", ID: "repoB", MovedFrom: "repoA"}},
		},
		{
			Event: plangateaudit.EventPhaseApproved, PhaseKey: "p2", PhaseIndex: &idx2,
			Ceiling: []string{pushHandle.String()}, Slots: []string{"git_repo"},
			SlotRefs: []plangateaudit.SlotRef{{Type: "git_repo", ID: "repoB", MovedFrom: "repoA"}},
		},
	}

	driveDecision(t, h, mem, ask, true)

	assert.Equal(t, 1, binder.moveCalls,
		"a single-occupancy pin moves at most once per approval; the later phase must bind same-instance, not re-move")
	assert.True(t, binder.wroteGrant("git_repo", "repoB", "read"), "the moving phase's read grant landed")
	assert.True(t, binder.wroteGrant("git_repo", "repoB", "push"),
		"the later phase's push grant must NOT be dropped by a spurious second move")
	assert.Equal(t, "repoB", binder.pinned["git_repo"], "the pin ends on the approved instance")
	for _, env := range published {
		assert.NotEqual(t, categories.InternalError, decodeInteractionRequest(t, env).Category,
			"a successful multi-phase move must not report an approval failure to the approver")
	}
}
