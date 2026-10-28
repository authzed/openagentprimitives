package goals

import (
	"context"
	"encoding/json"
)

// Mutation atomically commits goal state, a durable event, and a deduplication
// receipt. Hash commits to the caller's request before generated timestamps.
type Mutation struct {
	Goal      Goal
	Expected  int64 // zero for create
	RequestID string
	Hash      string
	Event     Event
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
