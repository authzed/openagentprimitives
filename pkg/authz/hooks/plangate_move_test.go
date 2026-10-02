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
