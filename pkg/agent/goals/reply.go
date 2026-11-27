package goals

import (
	"context"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
)

// A run reply's states. prepared -> attempted -> accepted|absent is the
// receiver-confirmed path every private reply and every group reply on a kind
// with a delivery receiver takes.
//
// unconfirmed is the hand-off path of a group reply on a kind without one:
// the reply was authorized and handed to the channel for posting through the
// ordinary outbound relay; delivery is not confirmed, and it never carries a
// receipt. As with any non-goal reply, a run cancelled between the hand-off
// and the relay's post still posts. publish_failed follows unconfirmed when
// the runner could not publish that envelope at all, so nothing was posted.
// accepted, absent, unconfirmed and publish_failed are terminal.
const (
	ReplyPrepared      = "prepared"
	ReplyAttempted     = "attempted"
	ReplyAccepted      = "accepted"
	ReplyAbsent        = "absent"
	ReplyUnconfirmed   = "unconfirmed"
	ReplyPublishFailed = "publish_failed"
)

// ErrReplyHandedOff refuses a second reply for a run whose reply was already
// handed off for posting. It is an ErrConflict.
var ErrReplyHandedOff = fmt.Errorf("%w: a reply for this run was already handed off for posting", ErrConflict)

// RunReply pins one bounded reply before transport acceptance. Sources are
// stamped by the service, never supplied by the agent.
type RunReply struct {
	Intent  delivery.Intent   `json:"intent"`
	Sources []Source          `json:"sources,omitempty"`
	State   string            `json:"state"` // one of the Reply* states above
	Receipt *delivery.Receipt `json:"receipt,omitempty"`
}

// Concluded reports whether the reply has reached a terminal state.
func (r RunReply) Concluded() bool {
	return r.State == ReplyAccepted || r.State == ReplyAbsent || r.State == ReplyUnconfirmed || r.State == ReplyPublishFailed
}

type ReplyStore interface {
	PrepareReply(context.Context, Occurrence, RunReply, time.Time) (Occurrence, error)
	// HandOffReply records a GROUP run's reply directly as ReplyUnconfirmed,
	// with the same fences as PrepareReply. The same operation repeated gets
	// the existing record back; any other reply for the run is ErrConflict.
	HandOffReply(context.Context, Occurrence, RunReply, time.Time) (Occurrence, error)
	// FailReplyPublish moves the handed-off reply naming operationID from
	// ReplyUnconfirmed to ReplyPublishFailed, under the same run fences.
	FailReplyPublish(ctx context.Context, o Occurrence, operationID string, now time.Time) (Occurrence, error)
	AttemptReply(context.Context, Occurrence, time.Time) (Occurrence, error)
	ConcludeReply(context.Context, Occurrence, *delivery.Receipt, time.Time) (Occurrence, error)
}
