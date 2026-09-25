package triggerdelivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Record stores the delivery that opened a session, once.
//
// The entry id (entryID) is a FIXED string, not content-derived — unlike
// systemprompt, whose id IS its digest, a conflict on this id proves only that
// SOMETHING is already recorded for this session; it could be the same
// delivery replayed (a provider redelivery, or a restarted channelsd
// reprocessing the same NATS message — the idempotent case this Kind depends
// on), or it could be a genuinely different delivery claiming to open the same
// session. Record distinguishes the two by reading the existing entry back and
// comparing bodies before deciding a conflict is a no-op. Swallowing every
// conflict unconditionally would let a different delivery vanish silently from
// an append-only audit trail whose entire purpose is being evidence.
//
// The Memory passed in MUST be the caller's provenance-signing facade; the
// append-only facade rejects unsigned writes.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	if len(c.Body) == 0 {
		// "There was a trigger but we kept nothing" is worse than no row: a
		// capture would read it as a recordable delivery and emit a bundle
		// whose payload is null.
		return fmt.Errorf("triggerdelivery.Record: refusing to record an empty body for %s", scope.ID)
	}
	if len(c.Body) > MaxBodyBytes {
		c.Body = c.Body[:MaxBodyBytes]
		c.Truncated = true
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("triggerdelivery.Record: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        entryID,
		CreatedAt: time.Now().UTC(),
		Content:   raw,
	})
	if errors.Is(err, memory.ErrAppendOnlyConflict) {
		// Do NOT assume idempotent — prove it. See the doc comment above for why
		// a conflict on this Kind's fixed id carries no such guarantee by itself.
		existing, rerr := Get(ctx, m, scope)
		if rerr == nil && existing != nil && bytes.Equal(existing.Body, c.Body) {
			return nil // genuinely the same delivery, already recorded
		}
		// "Could not verify" is NOT "genuinely different". Collapsing the two
		// makes the error assert something false: an operator greps for the
		// sentence below (channelsd logs it verbatim) and starts hunting a
		// second delivery that never existed. Say which one this was, and carry
		// the read failure so it is not lost.
		if rerr != nil {
			return fmt.Errorf("triggerdelivery.Record: session %s already has a recorded opening delivery "+
				"and this one could NOT be compared against it — the read-back failed, so whether the two "+
				"deliveries differ is UNKNOWN (read-back error: %v): %w", scope.ID, rerr, err)
		}
		if existing == nil {
			return fmt.Errorf("triggerdelivery.Record: session %s reported an append-only conflict but the "+
				"read-back found no existing delivery, so this one could not be compared against it: %w",
				scope.ID, err)
		}
		return fmt.Errorf("triggerdelivery.Record: session %s already has a recorded opening delivery "+
			"and this one is different: two different deliveries claimed to open one session: %w",
			scope.ID, err)
	}
	if err != nil {
		return fmt.Errorf("triggerdelivery.Record: %w", err)
	}
	return nil
}

// Get returns the delivery that opened the session, or (nil, nil) when there
// was none.
//
// (nil, nil) rather than an error: every session a person typed into is in this
// state, so treating absence as failure would make the common path an error
// path.
func Get(ctx context.Context, m memory.Memory, scope memory.Scope) (*Content, error) {
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}, IDs: []string{entryID}})
	if err != nil {
		return nil, fmt.Errorf("triggerdelivery.Get: %w", err)
	}
	if len(res.Entries) == 0 {
		return nil, nil
	}
	var c Content
	if err := json.Unmarshal(res.Entries[0].Content, &c); err != nil {
		return nil, fmt.Errorf("triggerdelivery.Get: undecodable entry %s in scope %s: %w",
			res.Entries[0].ID, scope.ID, err)
	}
	return &c, nil
}
