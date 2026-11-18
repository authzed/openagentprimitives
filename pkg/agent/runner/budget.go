package runner

import (
	"context"
	"log/slog"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

const (
	BudgetReasonTurns    = "MaxTurnsExceeded"
	BudgetReasonTokens   = "MaxTokensExceeded"
	BudgetReasonDuration = "MaxDurationExceeded"
	// BudgetReasonExpired marks a session past its wall-clock sessionExpiration.
	// The loop maps it to ReasonAgentSessionExpired (not the generic budget
	// reason) via budgetFailReason.
	BudgetReasonExpired = "SessionExpired"
)

// Budget tracks the active session's caps. MaxDuration is measured as ACTIVE
// run-time via runClock (excludes idle/human-wait, persists across sleep);
// SessionExpiration is a wall-clock lifetime cap measured from the session's
// StartedAt. Turns/tokens are unchanged (per active round).
type Budget struct {
	cfg              spiceboxv1alpha1.BudgetConfig
	runClock         *RunClock
	sessionStartedAt time.Time
	now              func() time.Time // injected for tests
}

// NewBudget builds a Budget. runClock supplies active run-time for MaxDuration
// (nil ⇒ duration unenforced — Elapsed is nil-safe). sessionStartedAt is the
// true session start (status.startedAt) for the SessionExpiration backstop
// (zero ⇒ unenforced, e.g. before StartedAt is stamped).
func NewBudget(cfg spiceboxv1alpha1.BudgetConfig, runClock *RunClock, sessionStartedAt time.Time) *Budget {
	return &Budget{cfg: cfg, runClock: runClock, sessionStartedAt: sessionStartedAt, now: time.Now}
}

// MaxDuration is the active run-time cap this budget enforces (0 ⇒ none).
// Nil-safe.
func (b *Budget) MaxDuration() time.Duration {
	if b == nil {
		return 0
	}
	return b.cfg.MaxDuration.Duration
}

// durationWatchFloor bounds how often watchDuration re-arms. While the run
// clock is paused (parked on a human) the remaining budget does not shrink, so
// without a floor a nearly-spent budget would re-arm a near-zero timer in a
// tight loop for the whole wait.
const durationWatchFloor = time.Second

// BudgetStatefulDrain is how long a stateful tool already in flight when
// maxDuration is reached may keep running before it is cancelled too (see
// interruptRegistry.cancelForBudget). It must stay well inside the operator's
// runDurationGrace (10m, pkg/controllers/agentsession/maxduration.go), which
// also has to cover the cancelled call unwinding, the 30s runDuration flush and
// the failure write; otherwise the operator reaps the pod under a tool that
// was still being allowed to finish. Exported only so the operator package can
// assert that ordering in a test (TestRunDurationGraceCoversTheStatefulDrain).
const BudgetStatefulDrain = 5 * time.Minute

// watchDuration blocks until ctx is done or active run-time reaches
// MaxDuration, then calls onExceeded once. Check only runs at turn boundaries,
// so this is what makes maxDuration a deadline on an in-flight LLM call or tool
// call rather than a bound checked after it returns. It measures the run
// clock, not wall-clock, so time parked on an approval or await_user_message
// inside a tool call is excluded exactly as Check excludes it. No-op when no
// cap or no clock is configured.
func (b *Budget) watchDuration(ctx context.Context, onExceeded func()) {
	if b.MaxDuration() <= 0 || b.runClock == nil {
		return
	}
	for {
		remaining := b.cfg.MaxDuration.Duration - b.runClock.Elapsed()
		if remaining <= 0 {
			onExceeded()
			return
		}
		t := time.NewTimer(max(remaining, durationWatchFloor))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Check returns "" when within budget; otherwise the first BudgetReason* tripped.
//
// turns is completed assistant turns; inputTokens/outputTokens are cumulative
// (only the sum is checked; cache_read is excluded by design). Duration is
// active run-time; expiration is wall-clock since session start.
func (b *Budget) Check(turns int32, inputTokens, outputTokens int64) string {
	if b.cfg.MaxTurns > 0 && turns >= b.cfg.MaxTurns {
		return BudgetReasonTurns
	}
	if b.cfg.MaxTokens > 0 && inputTokens+outputTokens >= b.cfg.MaxTokens {
		return BudgetReasonTokens
	}
	if b.cfg.MaxDuration.Duration > 0 && b.runClock.Elapsed() >= b.cfg.MaxDuration.Duration {
		return BudgetReasonDuration
	}
	if b.cfg.SessionExpiration.Duration > 0 && !b.sessionStartedAt.IsZero() &&
		b.now().Sub(b.sessionStartedAt) >= b.cfg.SessionExpiration.Duration {
		return BudgetReasonExpired
	}
	return ""
}

// cutOffForBudget is the watchdog's action once maxDuration is reached:
// cancel the LLM call and the stateless/readonly tools now, give in-flight
// stateful tools BudgetStatefulDrain to finish, then cancel any still running.
// Returns early if ctx (the Run's watch context) ends first, which is the
// normal case once the drained tools return and the loop fails the session.
func (l *Loop) cutOffForBudget(ctx context.Context) {
	sessRef := l.SessionKey.Namespace + "/" + l.SessionKey.Name
	drain := l.budgetStatefulDrain
	if drain <= 0 {
		drain = BudgetStatefulDrain
	}
	draining := l.interrupts.cancelForBudget(errMaxDurationExceeded)
	slog.Default().Info("maxDuration reached; cancelled the in-flight LLM call and stateless/readonly tools",
		"session", sessRef, "maxDuration", l.Budget.MaxDuration().String(),
		"drainingStatefulTools", draining, "drain", drain.String())
	if len(draining) == 0 {
		return
	}
	t := time.NewTimer(drain)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
	}
	if cut := l.interrupts.cancelRemainingForBudget(); len(cut) > 0 {
		slog.Default().Info("maxDuration drain window ran out; cancelled stateful tools still in flight",
			"session", sessRef, "tools", cut, "drain", drain.String())
	}
}
