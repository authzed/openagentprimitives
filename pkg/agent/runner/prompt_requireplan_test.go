package runner_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

// Whenever the gate is on, the agent has to be TOLD to plan. The prompt said
// only "When your work needs permissioned tools, declare `phases`" — a
// suggestion, with no hint that anything depends on it.
//
// Observed live twice. Under requirePlan an agent went straight to `git clone`
// and earned gate_would_deny on its first call, with nothing in its
// instructions explaining why. Under LOGGING mode — the mode that produces the
// dataset enforcement is gated on — an agent ran a full session, called
// update_plan zero times, and every one of its eight gate records read
// `gate_allowed` against the implicit phase. The gate rubber-stamped the run,
// and the dataset said nothing about what a real phase declaration looks like.
//
// The mandate therefore keys on the gate being ON, not on requirePlan. Only the
// CONSEQUENCE differs between the modes, and that difference is asserted below.
func TestComposeSystem_theGateBeingOnMakesPlanningMandatory(t *testing.T) {
	cases := []struct {
		name string
		opts []runner.ComposeOption
	}{
		{name: "requirePlan: mandate present", opts: []runner.ComposeOption{runner.WithRequirePlan()}},
		{name: "logging mode (no requirePlan): mandate still present", opts: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runner.ComposeSystem("fixture",
				[]tool.Tool{updatePlanTool(t), selectPhaseTool(t)}, nil, nil, nil, nil, tc.opts...)

			// The exact phrase, not a bare "MUST": the tool catalog already
			// contains one ("Pass the FULL current state..."), so a loose
			// substring check passes with no mandate present at all — which is
			// how the first draft of this test went green against the code it
			// was written to change.
			assert.Contains(t, got, "MUST declare phases with update_plan",
				"a precondition has to read as one; permissive wording produced a zero-plan session")
			assert.Contains(t, got, "BEFORE your first permissioned tool call",
				"the ordering is the whole point — plan BEFORE the first permissioned call")
		})
	}
}

// The consequence is where the modes diverge, and stating it wrongly is worse
// than stating nothing: in logging mode an unplanned call is NOT refused, it
// runs and is recorded against the implicit phase. An agent told it will fail
// learns that the threat is empty the first time it ignores it.
func TestComposeSystem_onlyRequirePlanClaimsAnUnplannedCallFails(t *testing.T) {
	withRequire := runner.ComposeSystem("fixture",
		[]tool.Tool{updatePlanTool(t), selectPhaseTool(t)}, nil, nil, nil, nil, runner.WithRequirePlan())
	loggingOnly := runner.ComposeSystem("fixture",
		[]tool.Tool{updatePlanTool(t), selectPhaseTool(t)}, nil, nil, nil, nil)

	assert.Contains(t, withRequire, "refuses any permissioned call",
		"under requirePlan the refusal is real and has to be stated")

	assert.NotContains(t, loggingOnly, "refuses any permissioned call",
		"logging mode does not refuse; claiming it does teaches the agent the instructions lie")
	assert.NotContains(t, loggingOnly, "fails the call",
		"same reason — no failure happens in logging mode")
}

// The gate off entirely: neither the guidance nor the mandate belongs, and
// prompt bloat has a real cost.
func TestComposeSystem_noGateMeansNoPhaseGuidanceAtAll(t *testing.T) {
	got := runner.ComposeSystem("fixture", nil, nil, nil, nil, nil, runner.WithRequirePlan())

	assert.NotContains(t, got, "Declaring phases",
		"requirePlan without the gate's tools is a contradiction; the guidance keys on the gate being on")
	assert.NotContains(t, got, "MUST declare phases",
		"an agent that will never be gated must not be told to plan")
}

// The agent never declared a slot, so no approval card ever named a resource.
//
// Observed live: the user's instruction contained the repository URL, the agent
// planned three phases, declared permissions for each, and declared zero slots.
// The card could therefore only say "this needs perm:fetch:git_repo" — a
// CATEGORY — so the user approved a kind of action rather than a repository,
// and every phase had to be decided separately.
//
// Nothing told it otherwise. The block explained phases and permissions and was
// silent on slots, while one bullet said "You do not need to know the concrete
// targets before you plan" — true when the target is genuinely unknown, and
// read as permission to never name one.
func TestComposeSystem_theGateAsksForTheResourceWhenItIsKnown(t *testing.T) {
	got := runner.ComposeSystem("fixture",
		[]tool.Tool{updatePlanTool(t), selectPhaseTool(t)}, nil, nil, nil, nil)

	assert.Contains(t, got, "slots",
		"an agent never told about slots will not declare one, and the card can then only name a type")
	assert.Regexp(t, `(?i)already know|the user (named|gave)`,
		got, "the instruction has to key on the target being KNOWN — that is the case being missed")
	assert.Regexp(t, `(?i)approve.*(once|now)|without (asking|interrupting)`, got,
		"the payoff is what makes an agent do this for its own reasons: one approval instead of one per phase")
}

// The honest half. An agent that cannot name a target must not invent one to
// win an earlier approval — a fabricated id would be authority, and it enters
// the digest.
func TestComposeSystem_anUnknownTargetIsDeclarableWithoutGuessing(t *testing.T) {
	got := runner.ComposeSystem("fixture",
		[]tool.Tool{updatePlanTool(t), selectPhaseTool(t)}, nil, nil, nil, nil)

	assert.Regexp(t, `(?i)(cannot|can't|do not|don't) know`, got,
		"the unknown-target case must stay explicitly allowed, or the mandate reads as 'always name something'")
}

// Telling the agent what to do did not work; the recorded prompt from a live
// session contained "Name the RESOURCE, not just the permission" verbatim and
// the agent declared zero slots. A refusal now forces the issue, but a refusal
// only tells it something is wrong — it still has to know the SHAPE to write.
//
// So the block carries one worked example, with real handles and a real slot,
// covering the case the agent is overwhelmingly in: the user already named the
// resource in their instruction.
func TestComposeSystem_carriesAWorkedPlanExample(t *testing.T) {
	got := runner.ComposeSystem("fixture",
		[]tool.Tool{updatePlanTool(t), selectPhaseTool(t)}, nil, nil, nil, nil,
		runner.WithPlannableSurface(exampleSurface(t)))

	assert.Regexp(t, `(?i)example`, got, "an example has to be recognizable as one")
	assert.Contains(t, got, `"slots"`,
		"the example must show the slots array — the field the agent kept omitting")
	// A PLACEHOLDER id, not a concrete one. The example is derived per session
	// now, so it cannot know a real instance — and a real-looking one would be
	// transcribed into plans it has nothing to do with, which is how a git URL
	// would reach a CRM agent. What must be unambiguous is that a value belongs
	// there and where it comes from.
	assert.Regexp(t, `"id"\s*:\s*"[^"]+"`, got,
		"the slot must show an id field with something in it")
	assert.Contains(t, got, "OMIT id entirely",
		"and show the deferral INSIDE the JSON: prose loses to the example, and a\n\t\t// placeholder describing where a value comes from got a display name\n\t\t// stamped in as a record id in every CRM trial")
	assert.Contains(t, got, `"handle"`, "permissions and slots shown together, as one phase carries both")
}

// The example must also show the deferral, or it teaches only the strict form —
// and an agent that has learned one shape and cannot use it will invent an id
// to match the example it was given.
func TestComposeSystem_theExampleShowsTheDeferredFormToo(t *testing.T) {
	got := runner.ComposeSystem("fixture",
		[]tool.Tool{updatePlanTool(t), selectPhaseTool(t)}, nil, nil, nil, nil,
		runner.WithPlannableSurface(exampleSurface(t)))

	assert.Regexp(t, `(?i)no id|without an id|omit the id`, got,
		"the honest 'I cannot name it yet' form has to be demonstrated, not just permitted")
}

// The agent cannot be told "do not invent handles" unless it has somewhere to
// look. describePlannableHandles renders the surface, but only as a NOTICE
// after a plan has already dropped handles — so the only way to learn the
// vocabulary was to get it wrong first.
//
// Observed live: an agent wrote raw tool names ("linear_list_projects") where
// handles ("tool:linear_list_projects") belong, and all ten were dropped
// silently. It had no way to know the difference at the time it planned.
func TestComposeSystem_listsTheDeclarableHandles(t *testing.T) {
	got := runner.ComposeSystem("fixture",
		[]tool.Tool{updatePlanTool(t), selectPhaseTool(t)}, nil, nil, nil, nil,
		runner.WithPlannableHandles([]string{"perm:fetch:git_repo", "perm:push:git_repo", "tool:apply_workspace"}))

	assert.Contains(t, got, "perm:fetch:git_repo", "the agent has to see the vocabulary it must use")
	assert.Contains(t, got, "tool:apply_workspace",
		"including the tool: form, which is the one it got wrong live")
	assert.Regexp(t, `(?i)do not (invent|guess|make up)`, got,
		"and be told plainly not to invent one")
	assert.Regexp(t, `(?i)dropped|ignored|silently`, got,
		"the consequence of inventing one is silent removal, which it cannot otherwise detect")
}

// Without the gate there is no surface and nothing to say. Listing handles for a
// session that gates nothing would be noise on every turn.
func TestComposeSystem_noHandleListWithoutTheGate(t *testing.T) {
	got := runner.ComposeSystem("fixture", []tool.Tool{updatePlanTool(t)}, nil, nil, nil, nil,
		runner.WithPlannableHandles([]string{"perm:fetch:git_repo"}))

	assert.NotContains(t, got, "perm:fetch:git_repo",
		"no select_phase means no plan gate, so the handle vocabulary is irrelevant")
}

// A gated session whose surface is empty must say so rather than print an empty
// list — "declare no permissions" is actionable; a blank line is not.
func TestComposeSystem_anEmptySurfaceSaysSoRatherThanListingNothing(t *testing.T) {
	got := runner.ComposeSystem("fixture",
		[]tool.Tool{updatePlanTool(t), selectPhaseTool(t)}, nil, nil, nil, nil,
		runner.WithPlannableHandles(nil))

	assert.Regexp(t, `(?i)no gated tools|nothing to declare|no declarable handles`, got,
		"an agent facing an empty surface must be told that, not left to infer it from silence")
}

// The toolkits' own planning guidance reaches the planner.
//
// That git_repo's read/write key on the checked-out copy while fetch/push key
// on the remote URL is a fact about GIT — nothing generic could know it, and
// the runner's prompt is shared by every toolkit. So the note is authored with
// the toolkit, published on the class, and rendered here, beside the handles
// it is about.
func TestComposeSystem_rendersToolkitPlanningNotes(t *testing.T) {
	got := runner.ComposeSystem("fixture",
		[]tool.Tool{updatePlanTool(t), selectPhaseTool(t)}, nil, nil, nil, nil,
		runner.WithPlannableHandles([]string{"perm:read:git_repo", "perm:write:git_repo"}),
		runner.WithPlanningNotes(map[string]string{
			"git_repo/write": "Declare perm:read:git_repo alongside it.",
		}))

	assert.Contains(t, got, "Declare perm:read:git_repo alongside it.",
		"the toolkit's guidance must reach the agent that plans")
	assert.Contains(t, got, "git_repo/write",
		"and be attributed to the permission it is about, not floated as generic advice")
}

// A session whose toolkits declared no notes renders no section — an empty
// heading with nothing under it is noise on every turn.
func TestComposeSystem_noPlanningNotesRendersNothing(t *testing.T) {
	got := runner.ComposeSystem("fixture",
		[]tool.Tool{updatePlanTool(t), selectPhaseTool(t)}, nil, nil, nil, nil,
		runner.WithPlannableHandles([]string{"perm:read:git_repo"}))

	assert.NotContains(t, got, "Notes on specific permissions")
}

// The worked example must not teach the mistake it exists to prevent.
//
// Its acting phase once declared write and push and NOT read — and an agent
// copied that shape verbatim, producing a plan that could commit but not read,
// which interrupted the user mid-task for the read. The example is the
// highest-leverage text in this section: whatever it shows is what gets copied.
func TestComposeSystem_planExampleDeclaresReadAlongsideWrite(t *testing.T) {
	got := runner.ComposeSystem("fixture",
		[]tool.Tool{updatePlanTool(t), selectPhaseTool(t)}, nil, nil, nil, nil,
		runner.WithPlannableSurface(exampleSurface(t)))

	acting := got[strings.LastIndex(got, `{"id":`):]
	require.NotEmpty(t, acting, "the worked example must still be present")
	assert.Contains(t, acting, "perm:read:ticket",
		"the phase that changes something must show the read it depends on")
	assert.Contains(t, acting, "perm:comment:ticket")
}

// exampleSurface is a small two-tier surface for the tests that assert on the
// worked plan example. It is deliberately NOT git-shaped: the example is
// derived per session now, and pinning these assertions to git would re-freeze
// the very coupling that removing the hardcoded example undid.
func exampleSurface(t *testing.T) []permsurface.Descriptor {
	t.Helper()
	mk := func(perm, resourceType string, impact authz.StateImpact) permsurface.Descriptor {
		h, err := permsurface.NewPermHandle(perm, resourceType)
		require.NoError(t, err)
		return permsurface.Descriptor{
			Handle: h, Permission: perm, ResourceType: resourceType, StateImpact: impact,
		}
	}
	return []permsurface.Descriptor{
		mk("read", "ticket", authz.Readonly),
		mk("comment", "ticket", authz.Readwrite),
	}
}

// An agent with NO plan gate gets none of the planning apparatus.
//
// The gate is opt-in (planGate.mode defaults to disabled) and most shipped
// agents leave it off: a CRM agent, a messaging agent, an SRE assistant. All of
// the planning text — the declarable-handle list, the toolkit planning notes,
// the derived example, a class's authored examples — hangs off select_phase,
// which is only offered when the gate is on.
//
// Asserted rather than reasoned about, because the failure is silent and
// expensive: it would put phase instructions and a worked plan into the prompt
// of an agent that has no update_plan to call, on every turn, forever.
func TestComposeSystem_noPlanGateRendersNoPlanningApparatus(t *testing.T) {
	got := runner.ComposeSystem("fixture",
		// No select_phase: the gate is off, exactly as a class that never
		// declares planGate resolves.
		[]tool.Tool{updatePlanTool(t)}, nil, nil, nil, nil,
		runner.WithPlannableHandles([]string{"perm:read:ticket"}),
		runner.WithPlannableSurface(exampleSurface(t)),
		runner.WithPlanningNotes(map[string]string{"ticket/comment": "declare read too"}),
		runner.WithAuthoredExamples([]runner.AuthoredExample{{
			Task:   "answer a ticket",
			Phases: []runner.AuthoredExamplePhase{{ID: "look", Permissions: []string{"perm:read:ticket"}}},
		}}))

	for _, absent := range []string{
		"ONLY declarable handles", // the surface list
		"Notes on specific permissions",
		"the shape that costs ONE approval", // the derived example
		"Worked examples for this agent",    // the authored ones
		"declare read too",                  // a note's own text
		"answer a ticket",                   // an authored example's task
	} {
		assert.NotContains(t, got, absent,
			"an agent with no plan gate must not be told how to declare phases")
	}
}
