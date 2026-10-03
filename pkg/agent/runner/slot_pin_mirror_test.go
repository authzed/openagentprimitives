package runner

// slot_pin_mirror_test.go covers the status.slotPins mirror wiring: that it
// reflects what SpiceDB actually holds (never what a caller asked to bind),
// that a plan-gate approval first-fill and move each land with the right
// shape, that the promote path mirrors through its real call site, and that
// an already-mirrored type costs no advisory read.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// recordingPinner is a stateful SlotBinder + SlotPinner: EnsurePin pins
// first-in-wins, MovePin repoints, and ReadPin reports the current pin while
// counting its calls — the count is what proves the already-mirrored skip.
type recordingPinner struct {
	pins      map[string]string // resourceType -> pinned id
	readCalls int
}

func (*recordingPinner) WriteRelationships(context.Context, []authz.Relation) error  { return nil }
func (*recordingPinner) DeleteRelationships(context.Context, []authz.Relation) error { return nil }
func (p *recordingPinner) EnsurePin(_ context.Context, resourceType, resourceID string, _ authz.SessionRef) (bool, string, error) {
	if p.pins == nil {
		p.pins = map[string]string{}
	}
	if cur, ok := p.pins[resourceType]; ok {
		return true, cur, nil
	}
	p.pins[resourceType] = resourceID
	return false, resourceID, nil
}
func (*recordingPinner) WriteGrantsPinned(context.Context, []authz.Relation, string, string, authz.SessionRef) error {
	return nil
}
func (p *recordingPinner) MovePin(_ context.Context, resourceType, _, toID string, _ []authz.Relation, _ authz.SessionRef) error {
	if p.pins == nil {
		p.pins = map[string]string{}
	}
	p.pins[resourceType] = toID
	return nil
}
func (p *recordingPinner) ReadPin(_ context.Context, resourceType string, _ authz.SessionRef) (string, error) {
	p.readCalls++
	return p.pins[resourceType], nil
}
func (*recordingPinner) ListGrantsFor(context.Context, string, string, authz.SessionRef) ([]authz.Relation, error) {
	return nil, nil
}

var _ authz.SlotPinner = (*recordingPinner)(nil)

// mirrorStatusPatcher builds a fake-client-backed StatusPatcher for ns/name,
// returning the client so a test can Get the session back.
func mirrorStatusPatcher(t *testing.T, ns, name string) (*StatusPatcher, ctrlclient.Client) {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	c := ctrlfake.NewClientBuilder().
		WithScheme(s).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	return NewStatusPatcher(c, types.NamespacedName{Namespace: ns, Name: name}), c
}

func slotPinsOf(t *testing.T, c ctrlclient.Client, ns, name string) []spiceboxv1alpha1.SlotPin {
	t.Helper()
	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: name}, &out), "Get session")
	return out.Status.SlotPins
}

// The review's false-positive case, end to end: a plan-gate approval whose
// one slot instance a Refused precondition DROPS reaches BindApproved, which
// returns nil having bound (and pinned) NOTHING. The mirror must reflect
// SpiceDB — no pin — not the bindings the call was asked to write. The
// Satisfied control proves the same wiring mirrors a pin that WAS written.
func TestNarrowToApproved_refusedPreconditionMirrorsNoSlotPin(t *testing.T) {
	cases := []struct {
		name       string
		isPublic   bool // the recorded fact; false → the precondition Refuses
		wantMirror bool
		why        string
	}{
		{
			name:       "Refused precondition: nothing pinned, nothing mirrored",
			isPublic:   false,
			wantMirror: false,
			why:        "BindApproved dropped the candidate and pinned nothing; a mirror entry would claim a pin SpiceDB never wrote",
		},
		{
			name:       "Satisfied precondition: the pinned instance is mirrored",
			isPublic:   true,
			wantMirror: true,
			why:        "the bound instance was pinned via EnsurePin, so the mirror must surface it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, mem := slotDecisionHost(t)
			h.l.PlanGateSlotPermissions = map[string]string{"crm_company": "contact_access"}
			h.l.AgentClass = classWithGatedCompany(`facts.observed.is_public == true`)
			recordCompanyFact(t, mem, "4210", tc.isPublic)

			pinner := &recordingPinner{}
			h.l.SlotBinder = pinner
			sp, c := mirrorStatusPatcher(t, "ns", "s")
			h.l.Status = sp

			ask := planPhaseAsk()
			ask.Payload["slots"] = []string{"crm_company"}
			ask.Payload["slotValues"] = map[string]string{"crm_company": "4210"}

			driveDecision(t, h, mem, ask, true)

			pins := slotPinsOf(t, c, "ns", "s")
			if !tc.wantMirror {
				assert.Empty(t, pins, tc.why)
				return
			}
			require.Len(t, pins, 1, tc.why)
			assert.Equal(t, "crm_company", pins[0].ResourceType)
			assert.Equal(t, "4210", pins[0].ResourceID)
			assert.Empty(t, pins[0].MovedBy, "a first fill is not a move")
			assert.Nil(t, pins[0].MovedAt)
		})
	}
}

// An approved MOVE that executes must mirror the repointed pin WITH its
// provenance: MovedBy names the approval record (its PhaseKey) and MovedAt
// the mirror time. Same scenario shape as the move-failure test beside it
// (host_approval_move_test.go), with a pinner whose MovePin succeeds.
func TestNarrowToApproved_executedMoveMirrorsThePinWithMovedBy(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	h := planGateHost(t, &channelevents.Envelope{})
	h.l.Mem = mem
	pinner := &recordingPinner{pins: map[string]string{"git_repo": "repoA"}}
	h.l.SlotBinder = pinner
	h.l.PlanGateSlotPermissionSets = map[string][]string{"git_repo": {"push"}}
	sp, c := mirrorStatusPatcher(t, "ns", "s")
	h.l.Status = sp

	pushHandle := mustPermHandle(t, "push", "git_repo")
	idx := int32(0)
	ask := planPhaseAsk()
	ask.Payload["phaseKey"] = "p1"
	ask.Payload["ceiling"] = []string{pushHandle}
	ask.Payload["covered"] = []plangateaudit.Content{{
		Event:      plangateaudit.EventPhaseApproved,
		PhaseKey:   "p1",
		PhaseIndex: &idx,
		Ceiling:    []string{pushHandle},
		Slots:      []string{"git_repo"},
		SlotRefs:   []plangateaudit.SlotRef{{Type: "git_repo", ID: "repoB", MovedFrom: "repoA"}},
	}}

	driveDecision(t, h, mem, ask, true)

	require.Equal(t, "repoB", pinner.pins["git_repo"], "the approved move must have executed")
	pins := slotPinsOf(t, c, "ns", "s")
	require.Len(t, pins, 1, "the moved pin must be mirrored")
	assert.Equal(t, "git_repo", pins[0].ResourceType)
	assert.Equal(t, "repoB", pins[0].ResourceID, "the mirror must show the instance the pin moved TO")
	assert.Equal(t, "p1", pins[0].MovedBy, "MovedBy must name the approval record's phase key")
	require.NotNil(t, pins[0].MovedAt, "a move must carry its timestamp")
}

// The promote path, through its REAL call site: one dispatch round of an
// observing tool records a fact, promoteObservedSlots binds and pins it, and
// the mirror at the end of the round surfaces exactly the pin ReadPin
// reports. Same harness as TestDispatch_ObservedFactBindsTheSlotWithoutA-
// FurtherTurn (loop_observed_binding_test.go), plus the SlotBinder + Status
// the mirror needs — delete the mirrorBoundSlotPins call in
// promoteObservedSlots and this fails with empty status.slotPins.
func TestDispatch_promotedObservedPinIsMirroredOntoStatus(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	w := &observedRelWriter{}
	mt := observedMCPTool(t, mem)
	l := observedBindingLoop(t, mem, w, []agenttool.Tool{mt})
	// The engine's grant writer doubles as the mirror's pinner, exactly as
	// production's spicedb RelationWriter does for both roles.
	l.SlotBinder = w
	sp, c := mirrorStatusPatcher(t, "default", "disp")
	l.Status = sp

	results := dispatchObservingTool(t, l, mt)
	require.False(t, results[0].IsError, "the observing tool must succeed; content=%q", results[0].Content)

	require.Len(t, w.wrote, 1, "the promotion must have bound the observed instance")
	pins := slotPinsOf(t, c, "default", "disp")
	require.Len(t, pins, 1, "the pin the promotion wrote must be mirrored within the same round")
	assert.Equal(t, "git_commit", pins[0].ResourceType)
	assert.Equal(t, "ba03f5969a", pins[0].ResourceID, "the mirror must report what ReadPin reports")
	assert.Empty(t, pins[0].MovedBy, "a promotion fill is never a move")
	assert.Nil(t, pins[0].MovedAt)
}

// The cost bound: a type already present in status.slotPins is skipped
// without an advisory read (pins change only through the move path, which
// refreshes the mirror itself), while an unmirrored sibling still reads once
// and lands. Multi-occupancy types are never read — they hold no pin.
func TestMirrorBoundSlotPins_alreadyMirroredTypeSkipsTheRead(t *testing.T) {
	ctx := context.Background()
	sp, c := mirrorStatusPatcher(t, "ns", "s")
	require.NoError(t, sp.RecordSlotPin(ctx,
		spiceboxv1alpha1.SlotPin{ResourceType: "git_repo", ResourceID: "repoA"}), "seed the mirrored entry")

	pinner := &recordingPinner{pins: map[string]string{"git_repo": "repoA", "crm_company": "4210"}}
	l := &Loop{
		Status:     sp,
		SlotBinder: pinner,
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
	}

	// Only the already-mirrored type attempted: zero reads.
	l.mirrorBoundSlotPins(ctx, []authz.BoundEntitySpec{{ResourceType: "git_repo", Permission: "push"}})
	assert.Zero(t, pinner.readCalls, "an already-mirrored type must not be re-read")

	// An unmirrored sibling reads exactly once; the mirrored one still skips;
	// a multi-occupancy type is never a candidate at all.
	l.mirrorBoundSlotPins(ctx, []authz.BoundEntitySpec{
		{ResourceType: "git_repo", Permission: "push"},
		{ResourceType: "crm_company", Permission: "contact_access"},
		{ResourceType: "log_stream", Permission: "read", Occupancy: authz.SlotOccupancyMulti},
	})
	assert.Equal(t, 1, pinner.readCalls, "only the unmirrored single-occupancy type is read")
	pins := slotPinsOf(t, c, "ns", "s")
	require.Len(t, pins, 2, "both single-occupancy pins mirrored, the multi type absent")
}

// mustPermHandle builds a permsurface handle string, failing the test on a
// malformed pair.
func mustPermHandle(t *testing.T, perm, resType string) string {
	t.Helper()
	h, err := permsurface.NewPermHandle(perm, resType)
	require.NoError(t, err)
	return h.String()
}
