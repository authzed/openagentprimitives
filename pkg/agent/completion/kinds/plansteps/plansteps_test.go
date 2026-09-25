package plansteps_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/plansteps"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
)

// sessionWithPlan builds a SessionContext whose plans store holds one plan
// named "main" with the given items.
func sessionWithPlan(t *testing.T, items ...plans.Item) *tool.SessionContext {
	t.Helper()
	// Operations is required by the plans store to auto-open an operation on a
	// pending→in_progress transition, which one of the cases below exercises.
	sess := &tool.SessionContext{
		Namespace: "default", Name: "review-1",
		State: state.NewRegistry(state.Deps{Operations: operations.New(nil, nil)}),
	}
	if len(items) > 0 {
		store, ok := plans.TryFrom(sess)
		require.True(t, ok, "the plans state kind must be registered")
		_, err := store.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: items})
		require.NoError(t, err, "seeding the plan")
	}
	return sess
}

// evaluate drives the requirement THROUGH the registry rather than by calling
// Check directly — the declared key has to resolve through completion.Get, the
// same single lookup every registered kind reaches its Check by.
func evaluate(t *testing.T, sess *tool.SessionContext) ([]completion.Unmet, error) {
	t.Helper()
	return completion.Evaluate(context.Background(),
		[]string{plansteps.Key}, completion.Input{Session: sess})
}

func TestPlanStepsComplete_PendingStepRefuses(t *testing.T) {
	sess := sessionWithPlan(t,
		plans.Item{ID: "s1", Label: "read the diff", Status: plans.StatusDone},
		plans.Item{ID: "s2", Label: "conclude the check run", Status: plans.StatusPending},
	)

	unmet, err := evaluate(t, sess)
	require.NoError(t, err)
	require.Len(t, unmet, 1, "a step the agent left pending is work it said it would do")
	assert.Equal(t, plansteps.Key, unmet[0].Key)
	assert.Contains(t, unmet[0].Missing, "s2", "must name the step still open")
	assert.Contains(t, unmet[0].Missing, "conclude the check run", "must quote its label so the model recognises it")
	assert.NotContains(t, unmet[0].Missing, "read the diff", "a finished step is not outstanding")
	assert.Contains(t, unmet[0].Missing, "update_plan", "must name the call that settles a step")
}

func TestPlanStepsComplete_InProgressStepRefuses(t *testing.T) {
	sess := sessionWithPlan(t, plans.Item{ID: "s1", Label: "render the report", Status: plans.StatusInProgress})

	unmet, err := evaluate(t, sess)
	require.NoError(t, err)
	require.Len(t, unmet, 1, "a step still running is the spinner the user is watching")
	assert.Contains(t, unmet[0].Missing, "in_progress")
}

func TestPlanStepsComplete_TerminalStepsSatisfy(t *testing.T) {
	cases := []struct {
		name   string
		status plans.Status
	}{
		// error is terminal on purpose: an agent whose only honest report is
		// that a step failed must still be able to finish the round.
		{name: "done: settled", status: plans.StatusDone},
		{name: "error: settled, the failure IS the outcome", status: plans.StatusError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := sessionWithPlan(t, plans.Item{ID: "s1", Label: "step", Status: tc.status})
			unmet, err := evaluate(t, sess)
			require.NoError(t, err)
			assert.Empty(t, unmet)
		})
	}
}

func TestPlanStepsComplete_NoPlanDeclaredSatisfies(t *testing.T) {
	unmet, err := evaluate(t, sessionWithPlan(t))
	require.NoError(t, err)
	assert.Empty(t, unmet, "declaring no plan is the ordinary single-step shape, not an empty promise")
}

func TestPlanStepsComplete_NoPlanStateFailsClosed(t *testing.T) {
	sess := &tool.SessionContext{Namespace: "default", Name: "review-1"} // no State registry
	_, err := evaluate(t, sess)
	require.Error(t, err,
		"a class that asked about plan state must not be told 'satisfied' by a session that carries none")
}

// A delegated child carries no plan of its OWN — planning tools are withheld
// from it — so plan-steps-complete is satisfied by design, not by the accident
// of an empty plan store. The short-circuit is intentional and robust: it holds
// even for a child with no plans store at all, which would otherwise hit the
// fail-closed "no plan state" error branch, wedging a child on a requirement it
// structurally cannot satisfy.
func TestPlanStepsComplete_DelegatedChildIsSatisfiedByDesign(t *testing.T) {
	child := &tool.SessionContext{Namespace: "default", Name: "child-1", IsDelegatedChild: true}
	f, err := plansteps.Requirement{}.Check(context.Background(), completion.Input{Session: child})
	require.NoError(t, err, "a delegated child must not error out on plan state it never had")
	assert.True(t, f.Met, "a delegated child is satisfied by design, regardless of plan-store availability")

	// A ROOT with no plan store is unchanged: it fails closed, because a root
	// that declared this requirement IS a planning session and a missing store
	// is a real fault to surface, not a child's structural absence of a plan.
	root := &tool.SessionContext{Namespace: "default", Name: "root-1"}
	_, err = plansteps.Requirement{}.Check(context.Background(), completion.Input{Session: root})
	require.Error(t, err, "a non-child with no plan state still fails closed, unchanged")
}
