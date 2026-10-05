package goals

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
)

// RunReply pins one bounded private reminder before transport acceptance.
// Sources are stamped by the service, never supplied by the agent.
type RunReply struct {
	Intent  delivery.Intent   `json:"intent"`
	Sources []Source          `json:"sources,omitempty"`
	State   string            `json:"state"` // prepared, attempted, accepted, absent
	Receipt *delivery.Receipt `json:"receipt,omitempty"`
}

type ReplyStore interface {
	PrepareReply(context.Context, Occurrence, RunReply, time.Time) (Occurrence, error)
	AttemptReply(context.Context, Occurrence, time.Time) (Occurrence, error)
	ConcludeReply(context.Context, Occurrence, *delivery.Receipt, time.Time) (Occurrence, error)
}
