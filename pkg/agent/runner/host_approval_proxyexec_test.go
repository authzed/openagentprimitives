package runner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
)

// runClockActive reports whether rc is currently accruing active time (i.e.
// NOT paused). Reads the unexported activeSince field directly under the
// clock's own lock — permissible from within package runner — rather than
// inferring pause state from an Elapsed() delta across a sleep, which would
// make the test timing-dependent.
func runClockActive(rc *RunClock) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.activeSince != nil
}

// awaitDecisionProbe drives host.AwaitDecision to completion for a single
// pending tool_call approval, capturing the live turn's RunClock/pause-depth
// state WHILE the await is blocked (durationPaused/duringDepth) and again
// AFTER the decision resolves (afterPaused/afterDepth).
func awaitDecisionProbe(t *testing.T, proxyExec bool) (duringActive bool, duringDepth int, afterActive bool, afterDepth int) {
	t.Helper()

	l := &Loop{
		RunClock: NewRunClock(0, time.Now),
		Approval: approval.New(),
	}
	host := newRunnerHost(l, hostSession{Namespace: "default", Name: "proxyexec-sess"})
	host.proxyExec = proxyExec

	const reqID = "req-proxyexec-probe"
	host.pending().store(reqID, &pendingApproval{kind: "tool_call"})

	type result struct {
		approved bool
		by       string
		timedOut bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		approved, by, timedOut, err := host.AwaitDecision(context.Background(), reqID, 5*time.Second)
		done <- result{approved, by, timedOut, err}
	}()

	// Wait until the orchestrator has registered the await (it does so inside
	// Await, which is called AFTER enterApprovalPause when the pause is not
	// skipped) — so by the time PendingForSession is non-zero, whatever this
	// call was going to do to the clock/pause-depth has already happened.
	sessionRef := host.sess.Namespace + "/" + host.sess.Name
	deadline := time.Now().Add(2 * time.Second)
	for l.Approval.PendingForSession(sessionRef) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for AwaitDecision to register the pending approval")
		}
		time.Sleep(time.Millisecond)
	}

	duringActive = runClockActive(l.RunClock)
	l.approvalPauseMu.Lock()
	duringDepth = l.approvalPauseDepth
	l.approvalPauseMu.Unlock()

	l.Approval.DeliverDecision(reqID, approval.Decision{Approved: true, ApproverID: "tester"})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for AwaitDecision to return after delivering the decision")
	}

	afterActive = runClockActive(l.RunClock)
	l.approvalPauseMu.Lock()
	afterDepth = l.approvalPauseDepth
	l.approvalPauseMu.Unlock()
	return duringActive, duringDepth, afterActive, afterDepth
}

// TestAwaitDecision_ProxyExec_SkipsSharedPause is the D-D3 regression test: a
// widget (proxy-exec) app-tool call's approval wait must NOT freeze the live
// agent turn's RunClock/progress/plan-card, because enterApprovalPause /
// exitApprovalPause are shared *Loop state a concurrent widget wait has no
// business touching.
func TestAwaitDecision_ProxyExec_SkipsSharedPause(t *testing.T) {
	t.Run("proxyExec=true: RunClock stays running, approvalPauseDepth untouched", func(t *testing.T) {
		duringActive, duringDepth, afterActive, afterDepth := awaitDecisionProbe(t, true)
		assert.True(t, duringActive, "a proxy-exec approval wait must NOT pause the live turn's RunClock")
		assert.Equal(t, 0, duringDepth, "a proxy-exec approval wait must NOT increment approvalPauseDepth")
		assert.True(t, afterActive, "RunClock must still be running after a proxy-exec approval resolves")
		assert.Equal(t, 0, afterDepth, "approvalPauseDepth must remain 0 for a proxy-exec approval")
	})

	t.Run("proxyExec=false: RunClock pauses for the LLM path, approvalPauseDepth round-trips", func(t *testing.T) {
		duringActive, duringDepth, afterActive, afterDepth := awaitDecisionProbe(t, false)
		assert.False(t, duringActive, "an LLM-path approval wait must pause the live turn's RunClock")
		assert.Equal(t, 1, duringDepth, "an LLM-path approval wait must increment approvalPauseDepth")
		assert.True(t, afterActive, "RunClock must resume once the LLM-path approval resolves")
		assert.Equal(t, 0, afterDepth, "approvalPauseDepth must return to 0 after the LLM-path approval resolves")
	})
}
