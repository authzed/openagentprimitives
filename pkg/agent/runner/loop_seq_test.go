package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// TestPackSeq_HigherTurnSortsAfterLowerTurn pins the ordering property the
// durable-index choice relies on: with the memory turn index (which survives a
// restart) as the high half, turn 7 of a resumed session sorts after turn 3.
//
// This asserts PackSeq alone — it constructs no Loop and never calls
// seqForEmit, so it cannot catch a runner that stops feeding PackSeq the
// durable index. That half is covered by
// TestEmitActivity_LoopSideSeqUsesSentinelBlockAndTurnIndex in
// loop_plan_activity_test.go.
func TestPackSeq_HigherTurnSortsAfterLowerTurn(t *testing.T) {
	resumedTurnPreCrash := channelevents.PackSeq(3, 1)
	resumedTurnPostCrash := channelevents.PackSeq(7, 1) // replay-continued index
	assert.Greater(t, resumedTurnPostCrash, resumedTurnPreCrash,
		"a resumed runner's events must sort AFTER pre-crash events")
}
