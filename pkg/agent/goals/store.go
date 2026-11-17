package goals

import (
	"context"
	"encoding/json"
	"fmt"
)

// Mutation atomically commits goal state, a durable event, and a deduplication
// receipt. Hash commits to the caller's request before generated timestamps.
type Mutation struct {
	Goal      Goal
	Expected  int64 // zero for create
	RequestID string
	Hash      string
	Event     Event
	// Prior is the goal as the writer read it, JSON-encoded. When set, the
	// commit is refused (ErrConflict) unless the stored goal still encodes
	// the same: the revision alone cannot detect an in-place change
	// (InPlaceStore), which keeps it.
	Prior json.RawMessage
}

// InPlaceStore commits a change at the goal's current revision: one that
// leaves what its approved runs may do alone, so the approval and every
// occurrence bound to that revision stay valid. Pending renewals, renewal
// decisions and manager changes are committed this way. m.Expected must equal
// m.Goal.Revision and m.Prior is required; the event joins the goal's
// sequenced execution audit, which is not keyed by revision. A store without
// it refuses such changes.
type InPlaceStore interface {
	CommitInPlace(ctx context.Context, m Mutation) (Goal, error)
}

// EncodePrior encodes g for Mutation.Prior.
func EncodePrior(g Goal) (json.RawMessage, error) {
	b, err := json.Marshal(g)
	if err != nil {
		return nil, fmt.Errorf("encode goal snapshot: %w", err)
	}
	return b, nil
}

type Store interface {
	Get(context.Context, Domain, string) (Goal, error)
	List(context.Context, Domain, ListRequest) (Page, error)
	Receipt(context.Context, Domain, string, string) (Goal, bool, error)
	Commit(context.Context, Mutation) (Goal, error)
	Pending(context.Context, int) ([]Event, error)
	// SaveEnvelope persists exactly what will be published before publication.
	SaveEnvelope(context.Context, string, json.RawMessage) error
	Envelope(context.Context, string) (json.RawMessage, error)
	Published(context.Context, string) error
}
