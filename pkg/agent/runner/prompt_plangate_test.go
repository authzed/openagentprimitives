package runner_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

func composeWith(t *testing.T, tools ...tool.Tool) string {
	t.Helper()
	return runner.ComposeSystem("you are a fixture agent", tools, nil, nil, nil, nil)
}

func updatePlanTool(t *testing.T) tool.Tool {
	t.Helper()
	return meta.NewUpdatePlan(meta.UpdatePlanConfig{})
}

func selectPhaseTool(t *testing.T) tool.Tool {
	t.Helper()
	return meta.NewSelectPhase(meta.SelectPhaseConfig{})
}

// Phase guidance appears only when the gate is on. select_phase is offered only
// then, so its presence IS the signal — an agent that will never be gated must
// not carry instructions for a feature it cannot use, and prompt bloat is a
// real cost paid on every single turn.
func TestComposeSystem_phaseGuidanceOnlyWhenTheGateIsOn(t *testing.T) {
	withoutGate := composeWith(t, updatePlanTool(t))
	withGate := composeWith(t, updatePlanTool(t), selectPhaseTool(t))

	assert.NotContains(t, withoutGate, "Declaring phases",
		"an ungated agent must not be told about phases")
	assert.Contains(t, withGate, "Declaring phases")
}

// The guidance has to carry the INCENTIVE, not just the rule. An agent told
// only "be narrow" cannot trade that against getting its work done; an agent
// told narrow is FASTER will choose it for its own reasons.
func TestComposeSystem_phaseGuidanceStatesTheIncentive(t *testing.T) {
	got := composeWith(t, updatePlanTool(t), selectPhaseTool(t))

	assert.Contains(t, got, "Narrow is the fast path",
		"the agent must learn that narrow is cheaper, not merely required")
	assert.Contains(t, strings.ToLower(got), "amendment",
		"and that UNDER-declaring has a cost too: an amendment interrupts the human again")
	assert.Contains(t, strings.ToLower(got), "complete",
		"narrow is about SCOPE, not about discovering permissions as you go")
}

// Everything an agent needs to actually use the feature must be present, or it
// will declare phases it cannot navigate.
func TestComposeSystem_phaseGuidanceCoversTheWholeLoop(t *testing.T) {
	got := composeWith(t, updatePlanTool(t), selectPhaseTool(t))

	for _, want := range []string{
		"phases",       // how to declare
		"why",          // the required justification
		"select_phase", // how to move
		"_reason",      // how to recover from a refusal
	} {
		assert.Contains(t, got, want, "phase guidance must cover %q", want)
	}
}

// A refusal must read as recoverable. An agent that believes a denial is
// terminal will give up or thrash instead of selecting the right phase.
func TestComposeSystem_phaseGuidanceFramesRefusalAsRecoverable(t *testing.T) {
	got := composeWith(t, updatePlanTool(t), selectPhaseTool(t))
	assert.Contains(t, got, "not a dead end")
}

// The recon-then-act shape resolves the bootstrap question an agent will
// otherwise stall on: how to plan before knowing what it is dealing with.
func TestComposeSystem_phaseGuidanceExplainsReconFirst(t *testing.T) {
	got := composeWith(t, updatePlanTool(t), selectPhaseTool(t))

	assert.Contains(t, got, "recon first")
	// "EVERY concrete target", not "the concrete targets". The original wording
	// was the only thing the block said about targets at all, so it read as
	// permission never to name one — and that is exactly what happened live: an
	// agent handed a repository URL declared three phases and zero slots. The
	// stall this guards against is still guarded; the sentence now says a target
	// you DO know is worth naming, which the slot bullet below spells out.
	assert.Contains(t, got, "You do not need to know EVERY concrete target before you plan",
		"otherwise an agent stalls on planning work it has not scoped yet")
}

// The prompt is paid for on every turn, so the block must stay small.
//
// The bound moved 1800 -> 1900 when the MUST-plan mandate became unconditional
// on the gate being on. That was paid for before it was asked for: the mandate
// and the old permissive "when your work needs permissioned tools" bullet said
// the same thing, so they were merged rather than stacked, and the per-mode
// consequence was folded into the same bullet instead of earning its own —
// 1951 down to 1855. The remaining 55 is a real increase, and it buys the one
// instruction whose absence produced a session that called update_plan zero
// times and logged eight implicit-phase allows.
//
// Raise this only for something that load-bearing, and trim before you do.
func TestComposeSystem_phaseGuidanceStaysCompact(t *testing.T) {
	withoutGate := composeWith(t, updatePlanTool(t))
	withGate := composeWith(t, updatePlanTool(t), selectPhaseTool(t))

	added := len(withGate) - len(withoutGate)
	require.Positive(t, added)
	// 1900 -> 2300 for the slot bullet, then -> 3200 for the worked example.
	// Neither could be merged: no other bullet mentions the instance axis at
	// all, which is precisely why an agent handed a repository URL declared zero
	// slots and the card could only name a category.
	//
	// The example is the larger cost and the better-evidenced one. The prose
	// version of this instruction is already explicit, a live session recorded
	// it verbatim in the prompt, and the agent still declared nothing — telling
	// it WHAT to do without showing the shape did not survive contact. The
	// nested `slots`-beside-`permissions` structure is exactly the kind that is
	// easy to read past in prose and unambiguous in an example.
	//
	// Both pay for themselves in the currency this bound protects. Without them
	// every phase costs its own approval round-trip — a card published, a human
	// interrupted, a decision awaited, and the whole prompt re-sent on the turn
	// after. Turning three approvals into one is cheaper than the turns it
	// removes, which is not true of most prompt text and is why the bound
	// exists at all. This block is now substantial; the next addition should
	// have to displace something rather than stack on it.
	//
	// 3200 -> 3300: the example's ship phase gained `perm:read:git_repo`. An
	// agent copied that phase verbatim without it, and stopped mid-task for a
	// read its plan should have carried. Per-toolkit guidance is what displaced
	// the generic bullet that would otherwise have said the same thing here.
	assert.Less(t, added, 3300,
		"phase guidance costs tokens on every turn; keep it tight")
}
