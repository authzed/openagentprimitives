package plans

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
)

func samplePhases() []Phase {
	return []Phase{
		{
			ID:    "recon",
			Label: "Read the tracker issue",
			Why:   "I need the issue body before I can decide what to change",
			Max:   &PhaseMax{Count: 1, Why: "one read is enough"},
			Permissions: []PhasePermission{
				{Handle: "perm:read:tracker_issue", Why: "to read the issue body"},
			},
		},
		{
			ID:    "write",
			Label: "Update the issue",
			Why:   "apply the fix the issue describes",
			Requires: []PhaseRequirement{
				{Phase: "recon", Why: "I must read the issue before editing it"},
			},
			Max: &PhaseMax{Count: 2, Why: "one edit, plus one retry if the first is rejected"},
			Permissions: []PhasePermission{
				{Handle: "perm:write:tracker_issue", Why: "to post the fix"},
			},
			Slots: []PhaseSlot{
				{Type: "tracker_issue", Why: "pin the single issue named in the request"},
			},
		},
	}
}

// Item.Phase is a PRESENTATION grouping: it puts items under a heading for the
// human. The gate never reads it — authorization keys on (frozen plan, index),
// never on anything an agent can name.
func TestItem_phaseIsPresentationOnly(t *testing.T) {
	it := Item{ID: "step-1", Label: "read it", Status: StatusPending, Phase: "recon"}

	b, err := json.Marshal(it)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"phase":"recon"`)

	var back Item
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, "recon", back.Phase)
}

func TestItem_phaseIsOmittedWhenUnset(t *testing.T) {
	b, err := json.Marshal(Item{ID: "step-1", Label: "x", Status: StatusPending})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "phase")
}

// Phases must survive the store's JSON round trip, or an approved plan cannot
// be reconstructed after a runner restart.
func TestPlan_phasesRoundTrip(t *testing.T) {
	in := Plan{Name: "main", Items: []Item{}, Phases: samplePhases()}

	b, err := json.Marshal(in)
	require.NoError(t, err)

	var out Plan
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in.Phases, out.Phases)
}

// A plan with no phases is the slice-1 shape and must keep working: absent
// phases means "one implicit phase", never "no authority".
func TestPlan_noPhasesIsValidAndOmitted(t *testing.T) {
	b, err := json.Marshal(Plan{Name: "main", Items: []Item{}})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "phases")

	var out Plan
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Nil(t, out.Phases)
}

// Max is a POINTER so "unset" is distinguishable from "explicitly 0". Absent
// means the default (count 1); a literal 0 would mean a phase that can never
// be entered, which the schema forbids but the type must not silently accept
// as the default.
func TestPhaseMax_nilIsDistinguishableFromZero(t *testing.T) {
	var unset Phase
	assert.Nil(t, unset.Max, "unset Max must stay nil, not become a zero struct")

	explicit := Phase{Max: &PhaseMax{Count: 0}}
	require.NotNil(t, explicit.Max)
	assert.Equal(t, 0, explicit.Max.Count)
}

// Every Why is agent-authored and untrusted. The type carries them so an
// approver and an auditor can read the agent's reasoning; nothing may parse
// them. This test exists to pin that they are plain strings with no structure
// the code could be tempted to interpret.
func TestPhase_everyWhyIsAPlainString(t *testing.T) {
	ph := samplePhases()[1]
	assert.IsType(t, "", ph.Why)
	assert.IsType(t, "", ph.Max.Why)
	assert.IsType(t, "", ph.Requires[0].Why)
	assert.IsType(t, "", ph.Permissions[0].Why)
	assert.IsType(t, "", ph.Slots[0].Why)
}

// Requires names another phase by the AGENT's id here, because this is the
// authored document. Freezing resolves ids to indices exactly once, so the
// gate never resolves a name at decision time.
func TestPhaseRequirement_carriesTheAuthoredID(t *testing.T) {
	ph := samplePhases()[1]
	require.Len(t, ph.Requires, 1)
	assert.Equal(t, "recon", ph.Requires[0].Phase)
}

// Phases must survive the persist → ReplayNote round trip, or an approved
// plan's phase list silently empties on a runner restart or idle re-hydrate.
//
// Both halves currently marshal the whole *Plan rather than rebuilding it
// field by field, so this passes today. It is pinned because the failure mode
// is silent: a field-by-field rewrite of either half would drop Phases with
// every other test still green. That is exactly how the resolved PlanGate was
// lost in settings.ToStatus: the resolver computed it and the conversion
// dropped it, with every unit test still passing.
func TestPlan_phasesSurviveTheReplayRoundTrip(t *testing.T) {
	var captured json.RawMessage
	s := NewStore(state.Deps{
		Operations: nil,
		AppendSystemNote: func(_ context.Context, note map[string]any) error {
			b, err := json.Marshal(note)
			require.NoError(t, err)
			var wrapped struct {
				Data json.RawMessage `json:"data"`
			}
			require.NoError(t, json.Unmarshal(b, &wrapped))
			captured = wrapped.Data
			return nil
		},
	})

	_, err := s.Update(context.Background(), "main", ParentRef{}, Content{
		Items:  []Item{{ID: "a", Label: "step", Status: StatusPending, Phase: "recon"}},
		Phases: samplePhases(),
	})
	require.NoError(t, err)
	require.NotEmpty(t, captured, "the upsert must have persisted a note")

	replayed := NewStore(state.Deps{})
	require.NoError(t, replayed.ReplayNote(captured))

	got, ok := replayed.Get("main")
	require.True(t, ok, "the replayed store must hold the plan")
	assert.Equal(t, samplePhases(), got.Phases, "phases must survive replay")
	require.Len(t, got.Items, 1)
	assert.Equal(t, "recon", got.Items[0].Phase, "the item's phase grouping must survive too")
}
