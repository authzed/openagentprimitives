// pkg/channels/channelsd/pipeline/content_inspection_phase_characterization_test.go
//
// Golden-masters where the content_inspection phase comes from, so a refactor
// cannot silently move the phase authority somewhere else.
package pipeline

import (
	"testing"

	"github.com/stretchr/testify/require"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
)

// TestContentInspectDecisionAskedDrivesAwaitingDecision_Characterization pins
// the single source of truth for AgentSession.status.phase == AwaitingDecision
// on a content_inspection approval: the runner's signed lifecycle projection.
// recordDecisionAsked (pkg/agent/runner/host_approval.go) emits
// lifecyclecore.DecisionAsked{Kind: DecisionContentInspect}, the pure core's
// Transition promotes State.Phase to PhaseAwaitingDecision (transition.go's
// transitionDecision, shared by all non-join decision kinds), and
// derivePhase/reconcilePhase (pkg/controllers/agentsession/phase.go) apply
// lifecyclecore.Project(state).Phase verbatim to the CR.
//
// channelsd is deliberately NOT a phase authority: its interaction surface is
// for rendering and dedup, and the generic park handler writes no phase
// condition of its own. A second writer of the phase would let the surface and
// the signed log disagree about whether the session is parked.
func TestContentInspectDecisionAskedDrivesAwaitingDecision_Characterization(t *testing.T) {
	base := lifecyclecore.State{Phase: lifecyclecore.PhaseRunning}

	next, effects := lifecyclecore.Transition(base, lifecyclecore.DecisionAsked{
		RequestID: "req-1",
		Kind:      lifecyclecore.DecisionContentInspect,
	})
	require.NotEmpty(t, effects, "DecisionAsked must produce log/status effects")

	view := lifecyclecore.Project(next)
	require.Equal(t, string(lifecyclecore.PhaseAwaitingDecision), view.Phase,
		"content_inspection DecisionAsked must park the projected phase at AwaitingDecision")

	// The projected condition is the wire-format signal the operator mirrors
	// onto AgentSession.status.conditions; pin the type string exactly so a
	// rename surfaces as a red test rather than silent drift.
	require.Len(t, view.Conditions, 1, "exactly one condition projects from the single pending decision")
	require.Equal(t, "ContentInspectionApprovalPending", view.Conditions[0].Type)
	require.Equal(t, "True", view.Conditions[0].Status)
}
