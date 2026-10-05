package parkedprompt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// entryID is the deterministic ID for a requestRef, so Note is idempotent and
// Resolve can address a single prompt without scanning. Hashed rather than
// concatenated because a requestRef is publisher-chosen and may contain
// characters that conflict with Memory.Put's ID validation.
func entryID(requestRef string) string {
	sum := sha256.Sum256([]byte(requestRef))
	return IDPrefix + hex.EncodeToString(sum[:16])
}

// Note records a render-ready prompt the session is parked on. Idempotent: the
// same requestRef with the same content re-Notes to the same entry. Callers
// pass ONLY ResurfaceCached categories — see the package doc for why a
// regenerate category must never be persisted.
//
// A RESOLVED record is never overwritten: Note reads before writing and leaves
// the tombstone standing. The same prompt legitimately arrives more than once —
// the re-surface path republishes an envelope it read as outstanding a moment
// earlier, and the runner re-emits its outstanding requests on restart — so a
// re-Note can land AFTER the decision that resolved it. No caller sets Resolved,
// so an unconditional Put would clear the tombstone, which is the only
// already-resolved verdict that outlives the process (the decision pipe reads
// this record directly). Clearing it lets a second click re-run a handler that
// is not replayable — a second SpiceDB grant tuple, a second Applied.
//
// There is deliberately no way to re-arm a resolved prompt: a requestRef is
// minted per request, so reusing a decided one is a collision, not a re-arm. A
// caller that genuinely needs to re-park a decided ref needs its own explicit
// accessor, not this one's side effect.
func Note(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	if c.RequestRef == "" {
		return fmt.Errorf("parkedprompt.Note: RequestRef is required")
	}
	id := entryID(c.RequestRef)
	// Fail-closed on a read error rather than writing blind: the caller logs and
	// carries on delivering (an unwritten record costs a re-surface; a cleared
	// tombstone costs the duplicate-decision guard).
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}, IDs: []string{id}})
	if err != nil {
		return fmt.Errorf("parkedprompt.Note: query: %w", err)
	}
	if len(res.Entries) > 0 {
		var existing Content
		if err := json.Unmarshal(res.Entries[0].Content, &existing); err != nil {
			// An unreadable record is not a tombstone — Outstanding and the
			// decision pipe both skip it, so it protects nothing and blocks
			// nothing. Overwrite it with this readable one, and say so.
			log.FromContext(ctx).Info("parkedprompt.Note: undecodable existing record; overwriting",
				"scope", scope.ID, "requestRef", c.RequestRef, "entry", res.Entries[0].ID, "err", err.Error())
		} else if existing.Resolved {
			return nil
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("parkedprompt.Note: marshal: %w", err)
	}
	if _, err := m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        id,
		CreatedAt: time.Now().UTC(),
		Content:   raw,
	}); err != nil {
		return fmt.Errorf("parkedprompt.Note: put: %w", err)
	}
	return nil
}

// Find reads the durable receipt for a request, including resolved tombstones.
// A publisher may use its presence to confirm that the relay retained a notice.
func Find(ctx context.Context, m memory.Memory, scope memory.Scope, requestRef string) (Content, bool, error) {
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}, IDs: []string{entryID(requestRef)}, Limit: 1})
	if err != nil {
		return Content{}, false, err
	}
	if res.Partial || res.Truncated {
		return Content{}, false, fmt.Errorf("parkedprompt.Find: incomplete receipt query")
	}
	if len(res.Entries) == 0 {
		return Content{}, false, nil
	}
	var c Content
	if err := json.Unmarshal(res.Entries[0].Content, &c); err != nil {
		return c, false, err
	}
	if c.RequestRef != requestRef {
		return c, false, fmt.Errorf("parkedprompt.Find: request reference mismatch")
	}
	return c, true, nil
}

// Outstanding returns every still-pending prompt for the scope, skipping
// resolved tombstones. A decode failure on one entry is skipped rather than
// failing the whole read: one unreadable record must not cost the user every
// other prompt they are waiting on.
func Outstanding(ctx context.Context, m memory.Memory, scope memory.Scope) ([]Content, error) {
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}})
	if err != nil {
		return nil, fmt.Errorf("parkedprompt.Outstanding: query: %w", err)
	}
	out := make([]Content, 0, len(res.Entries))
	for _, e := range res.Entries {
		var c Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			continue
		}
		if c.Resolved {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// Resolve marks the prompt for requestRef as no longer outstanding, so it is
// never re-surfaced. A tombstone rather than a delete: memory.Memory exposes no
// per-entry Delete (see Content.Resolved).
//
// Idempotent and safe for an unknown requestRef — the normal decision path and
// the gate-side timeout path both call it, and either may run first.
func Resolve(ctx context.Context, m memory.Memory, scope memory.Scope, requestRef string) error {
	if requestRef == "" {
		return nil
	}
	id := entryID(requestRef)
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}, IDs: []string{id}})
	if err != nil {
		return fmt.Errorf("parkedprompt.Resolve: query: %w", err)
	}
	if len(res.Entries) == 0 {
		return nil // never noted (a regenerate category, or already GC'd)
	}
	var c Content
	if err := json.Unmarshal(res.Entries[0].Content, &c); err != nil {
		return fmt.Errorf("parkedprompt.Resolve: decode: %w", err)
	}
	if c.Resolved {
		return nil
	}
	c.Resolved = true
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("parkedprompt.Resolve: marshal: %w", err)
	}
	if _, err := m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        id,
		CreatedAt: res.Entries[0].CreatedAt,
		Content:   raw,
	}); err != nil {
		return fmt.Errorf("parkedprompt.Resolve: put: %w", err)
	}
	return nil
}
