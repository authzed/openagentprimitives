package capability

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

func offeredNames(t *testing.T, env RunnerEnv) []string {
	t.Helper()
	tools, skip := planningCapability{}.Offer(OfferContext{Env: env})
	assert.Nil(t, skip, "planning is default-on and never skips")
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Name())
	}
	return out
}

// The phase tools are offered ONLY when the plan gate is on. With the gate off
// there is no frozen plan to index into, so they could do nothing but return an
// error — and an always-failing tool in the catalog is worse than an absent
// one: it spends the model.s attention and invites retries.
func TestPlanningCapability_offersPhaseToolsOnlyWhenTheGateIsOn(t *testing.T) {
	cases := []struct {
		name       string
		gateActive bool
		want       []string
	}{
		{"gate off: update_plan only", false, []string{"update_plan"}},
		{"gate on: update_plan, select_phase and complete_phase", true,
			[]string{"update_plan", "select_phase", "complete_phase"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, offeredNames(t, RunnerEnv{PlanGateActive: tc.gateActive}))
		})
	}
}

// update_plan is unconditional FOR A ROOT: planning is default-on, and a root
// agent must be able to declare a plan whether or not anything gates it. A
// delegated child is the exception — see the withholding test below.
func TestPlanningCapability_alwaysOffersUpdatePlan(t *testing.T) {
	assert.Contains(t, offeredNames(t, RunnerEnv{}), "update_plan")
}

// A delegated child is offered NO plan-authoring tool. A child that cannot
// author a plan cannot widen its own authority — structurally, not by policy —
// the same move already made for respond_to_user and agent_work_complete. It
// asks its parent (request_approval / request_plan_amendment) instead.
func TestPlanningCapability_withholdsFromDelegatedChild(t *testing.T) {
	child := &spiceboxv1alpha1.AgentSession{Spec: spiceboxv1alpha1.AgentSessionSpec{
		Parent: &spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "parent"},
	}}

	// Even with the plan gate ON — the case that would otherwise add
	// select_phase and complete_phase — a child gets nothing.
	tools, skip := planningCapability{}.Offer(OfferContext{
		Session: child,
		Env:     RunnerEnv{PlanGateActive: true},
	})

	assert.Empty(t, tools, "a delegated child must not be offered update_plan or any plan-authoring tool")
	require.NotNil(t, skip, "the withholding must state why, so it is auditable")
	assert.Equal(t, "planning", skip.Capability)
	assert.Contains(t, skip.Reason, "delegated child")
}

// A root (no Parent) is unaffected by the child guard: it still authors its plan.
func TestPlanningCapability_rootStillAuthorsItsPlan(t *testing.T) {
	root := &spiceboxv1alpha1.AgentSession{Spec: spiceboxv1alpha1.AgentSessionSpec{}}
	tools, skip := planningCapability{}.Offer(OfferContext{Session: root, Env: RunnerEnv{}})
	assert.Nil(t, skip)
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Name())
	}
	assert.Contains(t, out, "update_plan")
}

// The wiring neither tool's own tests can see: the capability builds them from
// RunnerEnv, and a field left unpassed leaves the guard inert while every
// unit test still passes.
//
// complete_phase must consult PhaseEntered — without it, a phase the runtime
// never saw entered could be declared complete, which is the bypass that would
// make completion WEAKER than the entry rule it extends.
func TestPlanningCapability_completePhaseConsultsPhaseEntered(t *testing.T) {
	var asked int
	tools, _ := planningCapability{}.Offer(OfferContext{Env: RunnerEnv{
		PlanGateActive: true,
		ActivePlan:     func(context.Context) (plangate.Plan, bool) { return onePhasePlan(t), true },
		PhaseEntered: func(context.Context) map[int]bool {
			asked++
			return map[int]bool{} // nothing entered
		},
	}})

	res := execByName(t, tools, "complete_phase", `{"index":0,"outcome":"claimed"}`)

	assert.Positive(t, asked, "the entered-check must be wired, not just constructed")
	assert.True(t, res.IsError, "an unentered phase cannot be completed")
}

// select_phase must consult PhaseCompletion, or a dependent phase opens with
// its prerequisite outstanding.
func TestPlanningCapability_selectPhaseConsultsPhaseCompletion(t *testing.T) {
	var asked int
	plan := onePhasePlan(t)
	plan.Phases = append(plan.Phases, plangate.Phase{
		Label: "Act", Max: plangate.MaxSpec{Count: 1},
		Requires: []plangate.RequiresEdge{{Phase: 0}},
	})

	tools, _ := planningCapability{}.Offer(OfferContext{Env: RunnerEnv{
		PlanGateActive: true,
		ActivePlan:     func(context.Context) (plangate.Plan, bool) { return plan, true },
		PhaseCompletion: func(context.Context) map[int]bool {
			asked++
			return map[int]bool{} // phase 0 unfinished
		},
		RecordPhaseSelection: func(context.Context, int) error { return nil },
	}})

	res := execByName(t, tools, "select_phase", `{"index":1}`)

	assert.Positive(t, asked, "the completion-check must be wired, not just constructed")
	assert.True(t, res.IsError, "phase 1 requires phase 0, which is unfinished")
}

func onePhasePlan(t *testing.T) plangate.Plan {
	t.Helper()
	return plangate.Plan{Phases: []plangate.Phase{
		{Label: "Recon", Max: plangate.MaxSpec{Count: 1}},
	}}
}

// execByName runs the named tool from an offered set.
func execByName(t *testing.T, tools []tool.Tool, name, args string) tool.Result {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() != name {
			continue
		}
		res, err := tl.Execute(context.Background(), json.RawMessage(args), nil)
		require.NoError(t, err)
		return res
	}
	require.FailNowf(t, "tool not offered", "%q not in the offered set", name)
	return tool.Result{}
}

// Same shape, for the entry limit: a SelectPhaseConfig field nothing assigns
// enforces nothing, and the enforcement is invisible from the tool's own tests.
func TestPlanningCapability_selectPhaseConsultsPhaseEntries(t *testing.T) {
	var asked int
	plan := onePhasePlan(t)
	plan.Phases = append(plan.Phases, plangate.Phase{Label: "Act", Max: plangate.MaxSpec{Count: 1}})

	tools, _ := planningCapability{}.Offer(OfferContext{Env: RunnerEnv{
		PlanGateActive: true,
		ActivePlan:     func(context.Context) (plangate.Plan, bool) { return plan, true },
		PhaseEntries: func(context.Context) map[int]int {
			asked++
			return map[int]int{1: 1} // already entered once, and max is 1
		},
		RecordPhaseSelection: func(context.Context, int) error { return nil },
	}})

	res := execByName(t, tools, "select_phase", `{"index":1}`)

	assert.Positive(t, asked, "the entry-count check must be wired, not just constructed")
	assert.True(t, res.IsError, "phase 1 was approved to run once and has already run once")
}
