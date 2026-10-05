package goals

import (
	"context"
	"time"
)

type OccurrenceState string

const (
	OccurrenceQueued    OccurrenceState = "queued"
	OccurrenceClaimed   OccurrenceState = "claimed"
	OccurrenceRunning   OccurrenceState = "running"
	OccurrenceUnknown   OccurrenceState = "unknown"
	OccurrenceSucceeded OccurrenceState = "succeeded"
	OccurrenceFailed    OccurrenceState = "failed"
	OccurrenceCancelled OccurrenceState = "cancelled"
)

// Occurrence persists the session name before any create attempt. Lease expiry
// only transfers worker ownership; it never forgets or replaces that session.
type Occurrence struct {
	ID            string          `json:"id"`
	Domain        Domain          `json:"domain"`
	GoalID        string          `json:"goalID"`
	GoalRevision  int64           `json:"goalRevision"`
	ConsentDigest string          `json:"consentDigest"`
	DueAt         time.Time       `json:"dueAt"`
	ExpiresAt     time.Time       `json:"expiresAt"`
	State         OccurrenceState `json:"state"`
	Worker        string          `json:"worker,omitempty"`
	Fence         int64           `json:"fence"`
	LeaseUntil    time.Time       `json:"leaseUntil,omitempty"`
	SessionName   string          `json:"sessionName"`
	SessionUID    string          `json:"sessionUID,omitempty"`
}

type ClaimRequest struct {
	ID         string
	Worker     string
	Now        time.Time
	Lease      time.Duration
	OwnerLimit int
	ClassLimit int
}

// OccurrenceStore is a durable-only dispatch ledger, separate from the goal
// management Store. Callers must pass Dispatchable before Schedule and Claim,
// and recheck authority at activation. Store transactions also compare current
// goal revision so management changes cannot race a claim into eligibility.
type OccurrenceStore interface {
	Schedule(context.Context, Goal) (Occurrence, error)
	Due(context.Context, time.Time, int) ([]Occurrence, error)
	Occurrence(context.Context, string) (Occurrence, error)
	Claim(context.Context, ClaimRequest) (Occurrence, error)
	Attach(context.Context, Occurrence, string, time.Time) (Occurrence, error)
	Renew(context.Context, Occurrence, time.Time, time.Duration) (Occurrence, error)
	// Finish requires authoritative terminal acknowledgement. Unknown keeps
	// the reservation until reconciliation determines whether effects occurred.
	Finish(context.Context, Occurrence, OccurrenceState, time.Time) (Occurrence, error)
}
