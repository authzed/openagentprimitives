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
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

type recordedCompletion struct {
	index   int
	outcome string
	called  int
}

func runCompletePhase(
	t *testing.T, plan plangate.Plan, entered map[int]bool,
	got *recordedCompletion, recErr error, args string,
) tool.Result {
	t.Helper()
	tl := meta.NewCompletePhase(meta.CompletePhaseConfig{
		ActivePlan: func(context.Context) (plangate.Plan, bool) { return plan, len(plan.Phases) > 0 },
		Entered:    func(context.Context) map[int]bool { return entered },
		Record: func(_ context.Context, i int, o string) error {
			got.index, got.outcome, got.called = i, o, got.called+1
			return recErr
		},
	})
	res, err := tl.Execute(context.Background(), json.RawMessage(args), nil)
	require.NoError(t, err)
	return res
}

// The ordinary path: an entered phase is declared done, with its outcome.
func TestCompletePhase_recordsTheDeclarationAndItsOutcome(t *testing.T) {
	var got recordedCompletion
	res := runCompletePhase(t, twoPhaseFrozen(t), map[int]bool{0: true}, &got, nil,
		`{"index":0,"outcome":"listed the open issues"}`)

	assert.False(t, res.IsError, "content=%s", res.Content)
	assert.Equal(t, 1, got.called)
	assert.Equal(t, 0, got.index)
	assert.Equal(t, "listed the open issues", got.outcome,
		"the outcome is what makes the trail self-documenting")
}

// THE guard. Declaring a phase complete that was never entered would let an
// agent satisfy a dependency without doing the prerequisite — weaker than the
// entry rule completion is meant to strengthen.
func TestCompletePhase_refusesAPhaseThatWasNeverEntered(t *testing.T) {
	var got recordedCompletion
	res := runCompletePhase(t, twoPhaseFrozen(t), map[int]bool{0: true}, &got, nil,
		`{"index":1,"outcome":"trust me"}`)

	require.True(t, res.IsError)
	assert.Contains(t, res.Content, "select_phase", "tell the agent the move that would work")
	assert.Zero(t, got.called, "nothing may reach the log")
}

// complete_phase is Passthrough and never itself gated — it can only withhold.
func TestCompletePhase_isPassthroughAndUngated(t *testing.T) {
	tl := meta.NewCompletePhase(meta.CompletePhaseConfig{})
	assert.Equal(t, authz.Passthrough, tl.Permission().StateImpact)
	assert.Equal(t, tool.KindMeta, tl.Kind())
}

// A completion that did not durably record did not happen — saying otherwise
// would leave the agent believing a dependent phase is unblocked when the fold
// will not agree.
func TestCompletePhase_recordFailureIsReportedNotSwallowed(t *testing.T) {
	var got recordedCompletion
	res := runCompletePhase(t, twoPhaseFrozen(t), map[int]bool{0: true}, &got,
		errors.New("memory unavailable"), `{"index":0,"outcome":"done"}`)

	require.True(t, res.IsError)
	assert.Contains(t, res.Content, "did not take effect")
	assert.Contains(t, res.Content, "memory unavailable", "the cause must not be swallowed")
}

func TestCompletePhase_rejectsAnOutOfRangeIndex(t *testing.T) {
	var got recordedCompletion
	res := runCompletePhase(t, twoPhaseFrozen(t), map[int]bool{0: true}, &got, nil,
		`{"index":7,"outcome":"done"}`)

	require.True(t, res.IsError)
	assert.Contains(t, res.Content, "out of range")
	assert.Zero(t, got.called)
}

// The schema demands an outcome: a completion nobody can read is a worse trail
// than no completion at all.
func TestCompletePhase_schemaRequiresAnOutcome(t *testing.T) {
	tl := meta.NewCompletePhase(meta.CompletePhaseConfig{})
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(tl.InputSchema(), &schema))

	assert.ElementsMatch(t, []string{"index", "outcome"}, schema.Required)
	assert.Contains(t, schema.Properties, "index")
	assert.NotContains(t, schema.Properties, "id", "phases are named by index, never by an agent-chosen id")
}

func TestCompletePhase_withNoApprovedPlanIsAnError(t *testing.T) {
	var got recordedCompletion
	res := runCompletePhase(t, plangate.Plan{}, nil, &got, nil, `{"index":0,"outcome":"done"}`)

	require.True(t, res.IsError)
	assert.Contains(t, res.Content, "no approved plan")
	assert.Zero(t, got.called)
}
