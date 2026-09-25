package authz

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	cst "github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	exs "github.com/authzed/openagentprimitives/pkg/memory/kinds/extraction_state"
)

// PollInterval is the default tick for WaitForExtraction's memory poll.
// Exposed for tests (override via the WithPollInterval option).
const PollInterval = 50 * time.Millisecond

// WaitForExtraction blocks until extraction_state{turnIndex:inboxIdx,
// status:complete|failed} appears in memory, or deadline elapses.
// Returns nil in both cases (best-effort wait; never errors a tool
// dispatch — the caller falls through to the next step regardless).
func WaitForExtraction(ctx context.Context, m memory.Memory, scope memory.Scope, inboxIdx int, deadline time.Duration) error {
	if m == nil {
		return nil
	}
	pollCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	tk := time.NewTicker(PollInterval)
	defer tk.Stop()
	for {
		c, ok, err := exs.ForTurn(pollCtx, m, scope, inboxIdx)
		if err == nil && ok && (c.Status == exs.StatusComplete || c.Status == exs.StatusFailed) {
			return nil
		}
		select {
		case <-pollCtx.Done():
			return nil
		case <-tk.C:
		}
	}
}

// WaitForColdStartTask blocks until a cold_start_task entry appears in memory
// (authzd's ColdStartHandler has recorded the approver's decision) or deadline
// elapses — the artifact the runner needs to place turn 0. Returns nil in both
// cases: best-effort wait; a timeout degrades to the runner's fail-open path
// (run the raw prompt). m==nil is a no-op.
//
// This is the ONLY cold-start wait. Waiting instead on a marker that says only
// "the request is in flight" would resume the runner before the decision it
// needs exists; the once-per-session cap on cold start likewise lives with the
// decision, in authzd's coldStartAlreadyDecided.
func WaitForColdStartTask(ctx context.Context, m memory.Memory, scope memory.Scope, deadline time.Duration) error {
	if m == nil {
		return nil
	}
	pollCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	tk := time.NewTicker(PollInterval)
	defer tk.Stop()
	for {
		_, ok, err := cst.Get(pollCtx, m, scope)
		if err == nil && ok {
			return nil
		}
		select {
		case <-pollCtx.Done():
			return nil
		case <-tk.C:
		}
	}
}
