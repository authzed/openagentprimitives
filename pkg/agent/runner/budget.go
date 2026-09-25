package runner

import (
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
