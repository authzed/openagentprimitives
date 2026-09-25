package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

func envelopeRec(handles ...string) plangateaudit.Content {
	return plangateaudit.Content{Event: plangateaudit.EventEnvelope, Ceiling: handles}
}

// The envelope is a BASELINE, not a bound. It costs nothing to record — no
// card, no tier, no human — because pricing it by breadth would reintroduce
// exactly the uninformed turn-0 approval that batch re-planning exists to
// eliminate.
func TestEnvelope_isRecordedNotApproved(t *testing.T) {
	st, err := Fold(threePhasePlan(t), []plangateaudit.Content{
		envelopeRec("perm:read:tracker_issue"),
	})
	require.NoError(t, err)

	assert.True(t, st.EnvelopeRecorded())
	assert.Empty(t, st.ApprovalsPending(), "recording an envelope asks nobody for anything")
}

func TestEnvelope_absentWhenNeverDeclared(t *testing.T) {
	st, err := Fold(threePhasePlan(t), nil)
	require.NoError(t, err)
	assert.False(t, st.EnvelopeRecorded())
}

// The whole point: a later plan reaching outside the pre-exposure baseline is a
// nameable event. Not proof of injection — an honest agent legitimately
// discovers work — but it is the SHAPE injection takes.
func TestEnvelope_detectsReachBeyondTheBaseline(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{
		envelopeRec("perm:read:tracker_issue"), // declared before any tool ran
	})
	require.NoError(t, err)

	assert.True(t, st.WithinEnvelope([]string{"perm:read:tracker_issue"}),
		"reach the pre-exposure self did anticipate is within the baseline")
	assert.False(t, st.LeavesEnvelope([]string{"perm:read:tracker_issue"}),
		"and therefore is not an exit")
	assert.True(t, st.LeavesEnvelope([]string{"perm:send:email"}),
		"reach the pre-exposure self never anticipated must be nameable")
}

func TestEnvelope_reachInsideTheBaselineDoesNotLeaveIt(t *testing.T) {
	st, err := Fold(threePhasePlan(t), []plangateaudit.Content{
		envelopeRec("perm:read:tracker_issue", "perm:write:tracker_issue", "perm:send:email"),
	})
	require.NoError(t, err)

	assert.False(t, st.LeavesEnvelope([]string{"perm:read:tracker_issue", "perm:send:email"}))
}

// With no envelope there is no baseline, so nothing can be judged against it —
// and the severity ladder treats every supersede as leaving it. That makes
// declaring one strictly better than not, with no switch to forget.
func TestEnvelope_noBaselineMeansEverythingLeavesIt(t *testing.T) {
	st, err := Fold(threePhasePlan(t), nil)
	require.NoError(t, err)

	assert.False(t, st.EnvelopeRecorded())
	assert.True(t, st.LeavesEnvelope([]string{"perm:read:tracker_issue"}),
		"no baseline ⇒ treated as left, so recording one is always the better choice")
}

// Only the FIRST envelope counts. An agent that has already read external text
// re-declaring a wider envelope would be laundering post-exposure reach into a
// "pre-exposure" baseline — which is the one thing the artifact must not allow.
func TestEnvelope_onlyTheFirstDeclarationCounts(t *testing.T) {
	st, err := Fold(threePhasePlan(t), []plangateaudit.Content{
		envelopeRec("perm:read:tracker_issue"),
		envelopeRec("perm:read:tracker_issue", "perm:send:email"), // a later, wider attempt
	})
	require.NoError(t, err)

	assert.True(t, st.LeavesEnvelope([]string{"perm:send:email"}),
		"a second, wider envelope must not widen the baseline")
}

// An envelope recorded with no handles is a real declaration of "I expect to
// need nothing", not an absent one — so it must not read as "no envelope".
func TestEnvelope_emptyDeclarationIsStillADeclaration(t *testing.T) {
	st, err := Fold(threePhasePlan(t), []plangateaudit.Content{envelopeRec()})
	require.NoError(t, err)

	assert.True(t, st.EnvelopeRecorded())
	assert.True(t, st.LeavesEnvelope([]string{"perm:read:tracker_issue"}),
		"everything is outside an empty envelope")
}

// Unparseable handles in the baseline must not silently widen it: a baseline
// that quietly admits everything it cannot read would suppress the very signal
// it exists to raise.
func TestEnvelope_unparseableHandlesDoNotWidenTheBaseline(t *testing.T) {
	st, err := Fold(threePhasePlan(t), []plangateaudit.Content{
		envelopeRec("perm:read:tracker_issue", "not a handle"),
	})
	require.NoError(t, err)

	assert.True(t, st.LeavesEnvelope([]string{"perm:send:email"}))
}
