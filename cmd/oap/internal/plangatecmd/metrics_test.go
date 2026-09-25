package plangatecmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

// The report exists to be READ by somebody deciding whether enforcement can be
// switched on, so the two facts that decision turns on have to be legible: how
// often enforcement would have refused a call, and which phases asked for reach
// they never used.
func TestRenderPlanGateReport_showsWhatTheEnforcementDecisionTurnsOn(t *testing.T) {
	var buf bytes.Buffer
	renderPlanGateReport(&buf, planGateReport{
		Session: "demo-agent-1",
		Digest:  "abcdef0123456789",
		Phases:  2,
		Metrics: plangate.Metric{
			GatedCalls: 10, WouldDeny: 3,
			DeclaredHandles: 5, ExercisedHandles: 2,
			ApprovalsByTier:      map[string]int{"0": 1, "1": 2},
			ExercisedNotDeclared: []string{"perm:write:doc"},
		},
		Drift: []plangate.PhaseUsage{
			{Index: 0, Label: "Recon", Entered: true,
				Declared: []string{"perm:read:doc", "perm:list:doc"}, Exercised: []string{"perm:read:doc"},
				DeclaredNeverExercised: []string{"perm:list:doc"}},
			{Index: 1, Label: "Act", Entered: false,
				Declared: []string{"perm:write:doc"}, DeclaredNeverExercised: []string{"perm:write:doc"}},
		},
	})
	got := buf.String()

	assert.Contains(t, got, "30%", "the would-deny RATE is the headline number")
	assert.Contains(t, got, "tier0=1 tier1=2")
	assert.Contains(t, got, "perm:write:doc",
		"reach used but never declared is what breaks on the day the default flips")
	assert.Contains(t, got, "Recon")
	assert.Contains(t, got, "perm:list:doc", "the per-phase never-used set is the ratchet's input")
	assert.Contains(t, got, "NO", "a phase that never ran must be visibly distinct from one that did")
}

// A phase with no label still needs a row: an unlabelled phase is exactly the
// kind an agent declares carelessly, so it is the last one to hide.
func TestRenderPlanGateReport_rendersAnUnlabelledPhase(t *testing.T) {
	var buf bytes.Buffer
	renderPlanGateReport(&buf, planGateReport{
		Session: "s", Digest: "d", Phases: 1,
		Metrics: plangate.Metric{},
		Drift:   []plangate.PhaseUsage{{Index: 0, Entered: false}},
	})

	assert.Contains(t, buf.String(), "(unlabelled)")
}

// Tier ordering is fixed rather than map iteration order: a report whose
// columns move between runs cannot be diffed across sessions, which is the
// whole point of collecting it.
func TestRenderTiers_isDeterministic(t *testing.T) {
	in := map[string]int{"2": 3, "0": 1, "1": 2}
	for i := 0; i < 8; i++ {
		assert.Equal(t, "tier0=1 tier1=2 tier2=3", renderTiers(in))
	}
}
