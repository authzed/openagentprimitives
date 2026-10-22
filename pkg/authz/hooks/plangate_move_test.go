package hooks

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// fakePinner is a SlotPinner whose only live method is ReadPin — the one the
// card build uses. The write methods are inert: the card never pins or moves,
// it only reads to RENDER a move.
type fakePinner struct {
	pins map[string]string // resourceType -> currently pinned (derived) id
	err  error             // injected into ReadPin
}

func (fakePinner) EnsurePin(context.Context, string, string, authz.SessionRef) (bool, string, error) {
	return false, "", nil
}
func (fakePinner) WriteGrantsPinned(context.Context, []authz.Relation, string, string, authz.SessionRef) error {
	return nil
}
func (fakePinner) MovePin(context.Context, string, string, string, []authz.Relation, authz.SessionRef) error {
	return nil
}
func (f fakePinner) ReadPin(_ context.Context, resourceType string, _ authz.SessionRef) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.pins[resourceType], nil
}
func (fakePinner) ListGrantsFor(context.Context, string, string, authz.SessionRef) ([]authz.Relation, error) {
	return nil, nil
}

// enforcingSlotGate is an enforcing gate whose one declared phase names a
// single-occupancy git_repo instance (repoB). Transforms are left nil, so the
// card's raw slot value is its own object id and the ReadPin comparison is a
// plain string match.
func enforcingSlotGate(t *testing.T, pinner authz.SlotPinner) *PlanGate {
	t.Helper()
	h, err := permsurface.NewPermHandle("push", "git_repo")
	require.NoError(t, err)
	plan := plangate.Plan{Phases: []plangate.Phase{{
		Label:       "work",
		Permissions: []permsurface.Handle{h},
		Max:         plangate.MaxSpec{Count: 1},
		Slots:       []plangate.Slot{{Type: "git_repo", ID: "repoB"}},
	}}}
	return NewPlanGate(PlanGateDeps{
		Mode:        "enforcing",
		Plan:        plan,
		CurrentPlan: func() (plangate.Plan, bool) { return plan, true },
		Surface:     []permsurface.Descriptor{permDesc(t, "push", "git_repo", "push_tool", authz.External)},
		SlotPinner:  pinner,
		Recorder:    &fakeRecorder{},
		Logger:      &fakeLogger{},
	})
}

// coveredFromPhaseAsk drives requestPhaseApproval and returns the covered-phase
// records carried on the resulting approval ask — the records a yes writes and
// planGateBindings later reads.
func coveredFromPhaseAsk(t *testing.T, h *PlanGate) []plangateaudit.Content {
	t.Helper()
	in := pipeline.Input{
		Point:   pipeline.PreToolCall,
		Session: pipeline.SessionRef{Namespace: "ns", Name: "s"},
		Tool:    &pipeline.ToolCallInfo{Name: "push_tool", UseID: "u1"},
	}
	d := h.requestPhaseApproval(context.Background(), in, plangateaudit.Content{Mode: "enforcing"}, 0, false)
	require.NotNil(t, d.Approval, "an un-denied enforcing phase must raise an approval ask")
	cov, ok := d.Approval.Payload["covered"].([]plangateaudit.Content)
	require.True(t, ok, "the ask must carry the covered-phase records")
	return cov
}

func firstSlotRef(t *testing.T, recs []plangateaudit.Content) plangateaudit.SlotRef {
	t.Helper()
	require.NotEmpty(t, recs)
	require.NotEmpty(t, recs[0].SlotRefs, "the covered phase must name its slot")
	return recs[0].SlotRefs[0]
}

// The MOVE case: the session is already pinned to a DIFFERENT instance than the
// card names, so the card records the displaced instance — approving it MOVES
// the pin.
func TestRequestPhaseApproval_recordsMovedFromWhenPinDiffers(t *testing.T) {
	h := enforcingSlotGate(t, fakePinner{pins: map[string]string{"git_repo": "repoA"}})
	ref := firstSlotRef(t, coveredFromPhaseAsk(t, h))
	assert.Equal(t, "repoB", ref.ID)
	assert.Equal(t, "repoA", ref.MovedFrom,
		"a card naming a different instance of a filled single-occupancy slot records the move")
}

// The pin is ALREADY the named instance: no move, MovedFrom empty.
func TestRequestPhaseApproval_noMovedFromWhenPinMatches(t *testing.T) {
	h := enforcingSlotGate(t, fakePinner{pins: map[string]string{"git_repo": "repoB"}})
	ref := firstSlotRef(t, coveredFromPhaseAsk(t, h))
	assert.Empty(t, ref.MovedFrom, "the card names the already-pinned instance, so nothing moves")
}

// An EMPTY slot (nothing pinned yet): a first-fill, MovedFrom empty.
func TestRequestPhaseApproval_noMovedFromOnFirstFill(t *testing.T) {
	h := enforcingSlotGate(t, fakePinner{pins: map[string]string{}})
	ref := firstSlotRef(t, coveredFromPhaseAsk(t, h))
	assert.Empty(t, ref.MovedFrom, "an empty slot is a first-fill, not a move")
}

// cardFromPhaseAsk drives requestPhaseApproval and returns the card the ask
// carries — what the approver is actually shown.
func cardFromPhaseAsk(t *testing.T, h *PlanGate) plangate.Card {
	t.Helper()
	in := pipeline.Input{
		Point:   pipeline.PreToolCall,
		Session: pipeline.SessionRef{Namespace: "ns", Name: "s"},
		Tool:    &pipeline.ToolCallInfo{Name: "push_tool", UseID: "u1"},
	}
	d := h.requestPhaseApproval(context.Background(), in, plangateaudit.Content{Mode: "enforcing"}, 0, false)
	require.NotNil(t, d.Approval)
	card, ok := d.Approval.Payload["card"].(plangate.Card)
	require.True(t, ok, "the ask must carry the card")
	return card
}

// A FIRST approval (no earlier approved phase, so no AddedSlots delta) must
// still SHOW the move it records. The card used to render the plan's own
// slots, which never carried MovedFrom, so the approver saw a plain first-fill
// while approving repointed the pin and revoked the old instance.
func TestRequestPhaseApproval_firstApprovalCardShowsTheMove(t *testing.T) {
	h := enforcingSlotGate(t, fakePinner{pins: map[string]string{"git_repo": "repoA"}})
	card := cardFromPhaseAsk(t, h)
	assert.Contains(t, card.What, "repoA → repoB", "the card must show the move it records")
	assert.Contains(t, card.What, "revokes this session's access to repoA",
		"the card must show the revocation approving it carries out")
}

// A `rebind: never` slot pinned to a DIFFERENT instance cannot move by approval,
// so it records no MOVE — but it must not render as a plain first-fill either,
// which would read to the approver as grantable. It is recorded immovable and
// the card says the named instance cannot be granted, with the new-session
// route. The bind is then refused by GrantSlots.
func TestRequestPhaseApproval_rebindNeverRendersBlockedNotGrantable(t *testing.T) {
	h := enforcingSlotGate(t, fakePinner{pins: map[string]string{"git_repo": "repoA"}})
	h.deps.SlotRebind = map[string]string{"git_repo": authz.SlotRebindNever}

	ref := firstSlotRef(t, coveredFromPhaseAsk(t, h))
	assert.Empty(t, ref.MovedFrom, "a never-rebind slot must not be offered as a move")
	assert.Equal(t, "repoA", ref.PinnedImmovable,
		"a never slot pinned elsewhere is recorded immovable so the card can say it can't be granted")

	card := cardFromPhaseAsk(t, h)
	assert.NotContains(t, card.What, "→", "a never slot must not render as a move")
	assert.Contains(t, card.What, "Cannot be granted", "the card must say the named instance can't be granted")
	assert.Contains(t, card.What, "pinned to repoA", "it must name the instance the pin is stuck on")
	assert.Contains(t, card.What, "Start a new session", "and the route out")
}

// No pinner wired (the unit fixtures, any binary with no SlotBinder): ReadPin is
// never called, so every card is a first-fill — never an error at card time.
func TestRequestPhaseApproval_nilPinnerLeavesMovedFromEmpty(t *testing.T) {
	h := enforcingSlotGate(t, nil)
	ref := firstSlotRef(t, coveredFromPhaseAsk(t, h))
	assert.Empty(t, ref.MovedFrom, "a nil pinner records no move")
}

// A ReadPin failure is advisory: MovedFrom stays empty (a first-fill card) and
// the card still builds, rather than failing at card time.
func TestRequestPhaseApproval_readPinErrorLeavesMovedFromEmpty(t *testing.T) {
	h := enforcingSlotGate(t, fakePinner{err: errors.New("spicedb unreachable")})
	ref := firstSlotRef(t, coveredFromPhaseAsk(t, h))
	assert.Empty(t, ref.MovedFrom, "an advisory read failure must not record a move, nor fail the card")
}

// threadMovedFrom carries a stamped move from the covered records onto the
// card's added slots by (type, id), so an amendment card renders a re-point as
// a move. A delta slot with no matching stamped move stays a first-fill, and
// the input delta is never mutated.
func TestThreadMovedFrom_copiesByTypeAndID(t *testing.T) {
	covered := []plangateaudit.Content{{SlotRefs: []plangateaudit.SlotRef{
		{Type: "crm_company", ID: "4299", MovedFrom: "4210"},
		{Type: "crm_company", ID: "7777"}, // no move recorded
	}}}
	in := []plangate.Slot{
		{Type: "crm_company", ID: "4299"},
		{Type: "crm_company", ID: "7777"},
	}

	out := threadMovedFrom(in, covered)

	require.Len(t, out, 2)
	assert.Equal(t, "4210", out[0].MovedFrom, "the re-pointed slot carries the stamped move")
	assert.Empty(t, out[1].MovedFrom, "a slot with no stamped move stays a first-fill")
	assert.Empty(t, in[0].MovedFrom, "the input delta must not be mutated")
}

// With nothing moved, threadMovedFrom returns the slots untouched — the common
// first-fill path pays nothing.
func TestThreadMovedFrom_noMoveIsNoOp(t *testing.T) {
	in := []plangate.Slot{{Type: "crm_company", ID: "4299"}}
	out := threadMovedFrom(in, []plangateaudit.Content{{SlotRefs: []plangateaudit.SlotRef{
		{Type: "crm_company", ID: "4299"},
	}}})
	require.Len(t, out, 1)
	assert.Empty(t, out[0].MovedFrom)
}

// threadMovedFrom also carries an IMMOVABLE pin (a `rebind: never` slot pinned
// elsewhere) onto the card's slots, matched by (type, id) — the same path
// MovedFrom takes — so the card can render it as a block rather than a grant.
func TestThreadMovedFrom_carriesPinnedImmovable(t *testing.T) {
	covered := []plangateaudit.Content{{SlotRefs: []plangateaudit.SlotRef{
		{Type: "git_repo", ID: "repoB", PinnedImmovable: "repoA"},
		{Type: "crm_company", ID: "4299", MovedFrom: "4210"}, // a move alongside, unaffected
	}}}
	in := []plangate.Slot{
		{Type: "git_repo", ID: "repoB"},
		{Type: "crm_company", ID: "4299"},
	}

	out := threadMovedFrom(in, covered)

	require.Len(t, out, 2)
	assert.Equal(t, "repoA", out[0].PinnedImmovable, "the immovable pin is carried onto the slot")
	assert.Empty(t, out[0].MovedFrom, "an immovable slot is not a move")
	assert.Equal(t, "4210", out[1].MovedFrom, "a move alongside is still carried")
	assert.Empty(t, out[1].PinnedImmovable)
	assert.Empty(t, in[0].PinnedImmovable, "the input delta must not be mutated")
}

// An amendment card renders only its delta, but a move can be stamped on a
// covered ref the delta does not contain — an unchanged slot whose pin has since
// moved elsewhere. Approving executes that move, so the card must show it: an
// amendment that adds only a handle still renders "<pinned> → <named>".
func TestWithUnshownPinChanges_amendmentShowsEveryMoveItExecutes(t *testing.T) {
	covered := []plangateaudit.Content{{SlotRefs: []plangateaudit.SlotRef{
		{Type: "crm_company", ID: "4299", MovedFrom: "4210"}, // unchanged slot, pin moved
		{Type: "git_repo", ID: "repoB", PinnedImmovable: "repoA"},
		{Type: "crm_contact", ID: "c1"}, // no pin change: stays off the delta
	}}}
	in := []plangate.Slot{{Type: "git_repo", ID: "repoB", PinnedImmovable: "repoA"}} // already shown

	out := withUnshownPinChanges(in, covered)

	require.Len(t, out, 2, "the unshown move is added; the shown block and the unchanged slot are not")
	assert.Equal(t, plangate.Slot{Type: "crm_company", ID: "4299", MovedFrom: "4210"}, out[1])
	assert.Len(t, in, 1, "the input delta must not be mutated")

	card := plangate.BuildCard(plangate.CardInput{
		Plan:         plangate.Plan{Phases: []plangate.Phase{{Label: "edit"}}},
		AddedHandles: []string{"perm:read:crm_contact"},
		AddedSlots:   out,
	})
	assert.Contains(t, card.What, "4210 → 4299", "the move the approval executes is on the card")
}
