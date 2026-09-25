package channel_msg_ref

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// entryID is the deterministic ID for a (kind, ref) tuple. The
// content (kind+ref+turnIndex) goes in Entry.Content; the ID is the
// hashed (kind, ref) so lookups don't require scanning every entry.
//
// Hashing rather than concatenating because refs can be arbitrarily
// long (Slack thread+ts in particular) and may contain characters
// that conflict with Memory.Put's ID-prefix validation.
func entryID(kind, ref string) string {
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{0}) // separator
	h.Write([]byte(ref))
	return IDPrefix + hex.EncodeToString(h.Sum(nil)[:16])
}

// Record stores an index entry mapping (kind, ref) → turnIndex at
// scope. Idempotent: re-Recording the same tuple with the same
// turnIndex is a no-op via Memory.Put's dedup.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, kind, ref string, turnIndex int) error {
	raw, err := json.Marshal(Content{Kind: kind, Ref: ref, TurnIndex: turnIndex})
	if err != nil {
		return fmt.Errorf("channel_msg_ref.Record: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        entryID(kind, ref),
		CreatedAt: time.Now().UTC(),
		Content:   raw,
	})
	return err
}

// Lookup returns the recorded turn index for (kind, ref), or
// (0, false, nil) if no entry exists. An error is returned only
// when the underlying memory query fails — a missing entry is a
// normal "not found" signal.
func Lookup(ctx context.Context, m memory.Memory, scope memory.Scope, kind, ref string) (int, bool, error) {
	id := entryID(kind, ref)
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}, IDs: []string{id}})
	if err != nil {
		return 0, false, fmt.Errorf("channel_msg_ref.Lookup: query: %w", err)
	}
	if len(res.Entries) == 0 {
		return 0, false, nil
	}
	var c Content
	if err := json.Unmarshal(res.Entries[0].Content, &c); err != nil {
		return 0, false, fmt.Errorf("channel_msg_ref.Lookup: decode: %w", err)
	}
	return c.TurnIndex, true, nil
}
