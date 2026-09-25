package sandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// TestStreamResultBudgetMatchesTheStdoutTail pins one budget read from both
// sides.
//
// A steel-thread capture carries a streaming toolkit's composed result and the
// replay serves it back as that toolkit's own stdout, where composeStreamResult
// folds at most maxStdoutTailBytes of it into the result it returns. The
// capture refuses a recording larger than bt.StreamResultBudgetBytes for
// exactly that reason, so the two numbers are one fact.
//
// Raise this constant alone and every capture at the old ceiling starts
// emitting a bundle whose replayed result is missing its HEAD — the tail buffer
// keeps the LAST bytes — and the derived assertion fails on text nobody can
// trace back here. Lower it alone and the capture refuses recordings the replay
// would have served fine.
func TestStreamResultBudgetMatchesTheStdoutTail(t *testing.T) {
	assert.Equal(t, maxStdoutTailBytes, bt.StreamResultBudgetBytes,
		"the capture's stream-result ceiling and the tail this function folds are one budget")
}
