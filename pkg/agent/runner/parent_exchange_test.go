package runner

// The run-time half of a delegated child's park: waiting on the agent that
// delegated to you is not you working, and must not spend your run-duration
// budget.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Brief test 4: the awaiting state does not burn status.runDuration.
//
// ask_parent parks through the same OnAwaitYield/OnAwaitResume hooks
// await_user_message uses (meta.yieldAndWait), so the property lives here: the
// yield stops the run clock and the resume restarts it. Remove the
// RunClock.Pause call from OnAwaitYield and this fails — nothing else in the
// suite would.
func TestOnAwaitYield_ParkedTimeDoesNotAccrueRunDuration(t *testing.T) {
	clk := &fakeClock{t: time.Unix(3000, 0)}
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: "default", Name: "delegated-child"},
		RunClock:   NewRunClock(0, clk.now),
	}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Some real work first, so the assertion is "the clock stopped", not "the
	// clock never started".
	clk.advance(10 * time.Second)
	require.Equal(t, 10*time.Second, l.RunClock.Elapsed(), "active work accrues run-time")

	l.OnAwaitYield(ctx)
	clk.advance(45 * time.Minute) // a long wait on a parent that took its time
	assert.Equal(t, 10*time.Second, l.RunClock.Elapsed(),
		"time spent waiting on the delegating agent is not this child working, and must not spend its maxDuration budget")

	l.OnAwaitResume(ctx)
	clk.advance(5 * time.Second)
	assert.Equal(t, 15*time.Second, l.RunClock.Elapsed(),
		"the answer landed: the child is working again and the budget resumes")
}
