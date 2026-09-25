package lifecycle

// TestContinuationDisposition_BudgetExhaustedRefusesNotInherits is a
// dedicated regression pin for the budget-exhaustion continuation mapping.
//
// Intended UX (why DispRefuse, not DispNewInheriting):
//   When a session terminates because it exhausted its token or cost budget
//   (FailureReason "BudgetExceeded"), a follow-up message from the user MUST
//   produce DispRefuse so channelsd posts a loud, user-visible notice naming the
//   reason. DispNewInheriting would be wrong here: without a deliberate budget
//   increase, a fresh inheriting session would hit the same ceiling on its first
//   LLM call and silently loop. The user needs to know to raise the budget
//   before retrying — the refuse disposition surfaces that requirement.
//
// A future reader who sees DispRefuse for BudgetExceeded and wonders "is this a
// bug — shouldn't a new session inherit and try again?" now has the answer in
// this test: no, it is intentional.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContinuationDisposition_BudgetExhaustedRefusesNotInherits(t *testing.T) {
	state := State{Phase: PhaseFailed, FailureReason: "BudgetExceeded"}
	got := ContinuationDisposition(state)

	// Must be DispRefuse — NOT DispNewInheriting. See package-level comment.
	disp, ok := got.(DispRefuse)
	require.True(t, ok,
		"BudgetExceeded must map to DispRefuse (not %T %v): "+
			"a new inheriting session would hit the same budget ceiling; "+
			"the user must raise the budget first",
		got, got)
	assert.Equal(t, "BudgetExceeded", disp.Reason,
		"DispRefuse.Reason must carry the exact failure reason "+
			"so channelsd can include it in the user-visible notice")
}
