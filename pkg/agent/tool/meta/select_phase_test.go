package meta_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

func twoPhaseFrozen(t *testing.T) plangate.Plan {
	t.Helper()
	h1, err := permsurface.NewPermHandle("read", "tracker_issue")
	require.NoError(t, err)
	h2, err := permsurface.NewPermHandle("write", "tracker_issue")
	require.NoError(t, err)

	return plangate.Plan{Phases: []plangate.Phase{
		{Label: "Recon", Permissions: []permsurface.Handle{h1}, Max: plangate.MaxSpec{Count: 1}},
		{Label: "Write", Permissions: []permsurface.Handle{h2}, Max: plangate.MaxSpec{Count: 1}},
	}}
}

type recordedSelection struct {
	index  int
	called int
}

// runSelectPhase builds the tool with a fake plan source and recorder, runs it,
// and returns the result. recErr simulates a failed durable write.
func runSelectPhase(t *testing.T, plan plangate.Plan, planOK bool, got *recordedSelection, recErr error, args string) tool.Result {
	t.Helper()
	tl := meta.NewSelectPhase(meta.SelectPhaseConfig{
		ActivePlan: func(context.Context) (plangate.Plan, bool) { return plan, planOK },
		Record: func(_ context.Context, idx int) error {
			if recErr != nil {
				return recErr
			}
			got.called++
			got.index = idx
			return nil
		},
	})
	res, err := tl.Execute(context.Background(), json.RawMessage(args), nil)
	require.NoError(t, err, "Execute must not return a Go error for agent-input problems")
	return res
}

// select_phase must be Passthrough: it never appears on the permission surface
// and is never itself gated. It cannot widen anything — every index it can name
// leads to a ceiling a human already approved.
func TestSelectPhase_isPassthroughAndUngated(t *testing.T) {
	tl := meta.NewSelectPhase(meta.SelectPhaseConfig{})
	assert.Equal(t, authz.Passthrough, tl.Permission().StateImpact)
	assert.Empty(t, tl.PermissionVariants())
	assert.Equal(t, "select_phase", tl.Name())
}

// It takes an INDEX into the frozen list, not a name. An id-based tool would
// hand the agent back the naming authority freezing deliberately removed.
func TestSelectPhase_schemaTakesAnIndexNotAnID(t *testing.T) {
	var schema map[string]any
	require.NoError(t, json.Unmarshal(meta.NewSelectPhase(meta.SelectPhaseConfig{}).InputSchema(), &schema))

	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)

	idx, ok := props["index"].(map[string]any)
	require.True(t, ok, "the tool must take an index")
	assert.Equal(t, "integer", idx["type"])
	assert.EqualValues(t, 0, idx["minimum"])

	assert.NotContains(t, props, "id", "an id-based selector would reintroduce agent-chosen identity")
	assert.NotContains(t, props, "phase")
	assert.Equal(t, false, schema["additionalProperties"])
}

func TestSelectPhase_recordsTheSelection(t *testing.T) {
	plan := twoPhaseFrozen(t)
	var got recordedSelection

	res := runSelectPhase(t, plan, true, &got, nil, `{"index":1}`)

	require.False(t, res.IsError, "%s", res.Content)
	assert.Equal(t, 1, got.called, "the agent supplies WHICH; the runtime records THAT IT HAPPENED")
	assert.Equal(t, 1, got.index)
	assert.Contains(t, res.Content, "Write", "the reply names the phase so the agent can confirm it")
}

func TestSelectPhase_rejectsAnOutOfRangeIndex(t *testing.T) {
	cases := []struct{ name, args string }{
		{"index past the end", `{"index":2}`},
		{"far past the end", `{"index":9999}`},
		{"negative index", `{"index":-1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name+": rejected without recording", func(t *testing.T) {
			var got recordedSelection
			res := runSelectPhase(t, twoPhaseFrozen(t), true, &got, nil, tc.args)

			assert.True(t, res.IsError)
			assert.Zero(t, got.called, "an invalid selection must not reach the log")
			assert.Contains(t, res.Content, "2 phases", "the error tells the agent the valid range")
		})
	}
}

// With no approved plan there is no frozen list to index into, so selection is
// meaningless rather than defaulting to phase 0.
func TestSelectPhase_withNoApprovedPlanIsAnError(t *testing.T) {
	var got recordedSelection
	res := runSelectPhase(t, plangate.Plan{}, false, &got, nil, `{"index":0}`)

	assert.True(t, res.IsError)
	assert.Zero(t, got.called)
	assert.Contains(t, res.Content, "no approved plan")
}

// A failed record must NOT report success: the agent would believe it holds a
// ceiling the log never granted, and the fold would disagree with it.
func TestSelectPhase_recordFailureIsReportedNotSwallowed(t *testing.T) {
	var got recordedSelection
	res := runSelectPhase(t, twoPhaseFrozen(t), true, &got, errors.New("memory unavailable"), `{"index":1}`)

	assert.True(t, res.IsError, "a selection that was not durably recorded did not happen")
	assert.Contains(t, res.Content, "memory unavailable")
}

func TestSelectPhase_malformedArgsAreRejected(t *testing.T) {
	var got recordedSelection
	res := runSelectPhase(t, twoPhaseFrozen(t), true, &got, nil, `{"index":"one"}`)

	assert.True(t, res.IsError)
	assert.Zero(t, got.called)
}

// Leaving work unfinished elsewhere is allowed: an agent that finds a phase
// unnecessary must not be wedged into completing it. Only a DEPENDENCY blocks.
func TestSelectPhase_opensAPhaseThatDependsOnNothingWithWorkOutstanding(t *testing.T) {
	plan := twoPhaseFrozen(t) // phase 1 requires nothing
	var got recordedSelection

	res := runSelectPhaseWithCompletion(t, plan, &got, map[int]bool{0: false}, `{"index":1}`)

	assert.False(t, res.IsError, "content=%s", res.Content)
	assert.Equal(t, 1, got.called, "the selection is recorded like any other")
}

// The one blocked case: the target requires a phase that has not finished.
func TestSelectPhase_refusesAPhaseWhosePrerequisiteIsUnfinished(t *testing.T) {
	plan := twoPhaseFrozen(t)
	plan.Phases[1].Requires = []plangate.RequiresEdge{{Phase: 0}}
	var got recordedSelection

	res := runSelectPhaseWithCompletion(t, plan, &got, map[int]bool{0: false}, `{"index":1}`)

	require.True(t, res.IsError)
	assert.Contains(t, res.Content, "Recon", "name the phase holding it up")
	assert.Zero(t, got.called,
		"a refused transition must not be recorded, or the fold would move the "+
			"agent to a phase the gate just refused to open")
}

// Finishing the prerequisite releases it.
func TestSelectPhase_opensADependentPhaseOnceItsPrerequisiteIsDone(t *testing.T) {
	plan := twoPhaseFrozen(t)
	plan.Phases[1].Requires = []plangate.RequiresEdge{{Phase: 0}}
	var got recordedSelection

	res := runSelectPhaseWithCompletion(t, plan, &got, map[int]bool{0: true}, `{"index":1}`)

	assert.False(t, res.IsError, "content=%s", res.Content)
	assert.Equal(t, 1, got.called)
}

// Without a completion source there is nothing to enforce against. The tool
// must keep working — a missing dep is a wiring gap, and turning it into "every
// dependent phase is blocked" would wedge every session that has one.
func TestSelectPhase_withNoCompletionSourceStillOpensDependentPhases(t *testing.T) {
	plan := twoPhaseFrozen(t)
	plan.Phases[1].Requires = []plangate.RequiresEdge{{Phase: 0}}
	var got recordedSelection

	res := runSelectPhase(t, plan, true, &got, nil, `{"index":1}`)

	assert.False(t, res.IsError, "content=%s", res.Content)
}

// runSelectPhaseWithCompletion builds the tool with a completion source wired.
func runSelectPhaseWithCompletion(
	t *testing.T, plan plangate.Plan, got *recordedSelection, complete map[int]bool, args string,
) tool.Result {
	t.Helper()
	tl := meta.NewSelectPhase(meta.SelectPhaseConfig{
		ActivePlan: func(context.Context) (plangate.Plan, bool) { return plan, true },
		Record: func(_ context.Context, i int) error {
			got.index, got.called = i, got.called+1
			return nil
		},
		PhaseCompletion: func(context.Context) map[int]bool { return complete },
	})
	res, err := tl.Execute(context.Background(), json.RawMessage(args), nil)
	require.NoError(t, err)
	return res
}
