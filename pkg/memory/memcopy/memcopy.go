// Package memcopy provides the turn-anchored memory-copy primitive the
// AgentSession controller's restart reconciler uses to fork a new session from
// an existing one.
//
// CopyPrefix copies entries whose "turn anchor" (a Link of Kind "turn") points
// at a turn Index ≤ cutTurn. Entries with no anchor are copied unconditionally:
// they are session-level state (lifecycle, authz_session_config, …) that pins
// to no turn.
//
// Idempotent: copies carry the SOURCE entry's content + CreatedAt verbatim, so
// a re-copy is a Put the append-only facade treats as a no-op. That survives a
// durable backend only because the facade compares CANONICAL forms
// (memory.CanonicalContent / CanonicalTime) — postgres re-emits content from a
// JSONB column and created_at from a TIMESTAMPTZ one, so the stored entry a
// re-copy is checked against is never byte-identical to the one written.
//
// CAUTION — ORDERING: turn (and the other append-only kinds) reject a
// content-changing Put with ErrAppendOnlyConflict; Put is NOT
// last-writer-wins. A child scope must therefore be seeded via CopyPrefix
// BEFORE anything else (e.g. a forked session's runner) can write a divergent
// turn at the same deterministic ID — see
// pkg/controllers/agentsession/restart.go.
package memcopy

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// CopyPrefix copies entries from src to dst keeping only those whose
// turn anchor points at Index ≤ cutTurn (entries with no anchor are
// copied unconditionally).
func CopyPrefix(ctx context.Context, m memory.Memory, src, dst memory.Scope, cutTurn int) error {
	res, err := m.Query(ctx, memory.Query{Scope: src})
	if err != nil {
		return fmt.Errorf("memcopy.CopyPrefix: query src: %w", err)
	}
	for _, e := range res.Entries {
		// Some Kinds are a CHAIN, not a set: their meaning comes from order and
		// from the per-(scope, publisher) provenance sequence. Copying those
		// into a new scope produces a history the child does not have — seq and
		// prevHash referring to entries that are not there — so the Kind opts
		// out and the forking code derives what the child needs instead.
		//
		// LookupKind cannot miss here in practice — the facade refuses a Put for
		// an unregistered Kind, so nothing unregistered can be in a scope — but
		// the ok-guard keeps an unclassifiable entry COPYING rather than being
		// dropped. That is the safe direction: the risk this property addresses
		// is a mis-ordered replay, not the presence of data.
		if k, ok := memory.LookupKind(e.Kind); ok && k.Retention().NeverForkCopy {
			continue
		}

		keep, kerr := keepForCut(e, cutTurn)
		if kerr != nil {
			return fmt.Errorf("memcopy.CopyPrefix: classify %q: %w", e.ID, kerr)
		}
		if !keep {
			continue
		}
		copyEntry := memory.Entry{
			Scope:      dst,
			Kind:       e.Kind,
			ID:         e.ID,
			CreatedAt:  e.CreatedAt,
			Content:    append([]byte(nil), e.Content...),
			Links:      append([]memory.Link(nil), e.Links...),
			Provenance: copyProvenance(e.Provenance),
		}
		if _, perr := m.Put(ctx, copyEntry); perr != nil {
			return fmt.Errorf("memcopy.CopyPrefix: put %q: %w", e.ID, perr)
		}
	}
	return nil
}

// copyProvenance deep-copies p (nil stays nil), so the copy's signature bytes
// never alias the source entry's.
func copyProvenance(p *memory.Provenance) *memory.Provenance {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Sig = append([]byte(nil), p.Sig...)
	return &cp
}

// keepForCut decides whether an entry should be copied given the
// cut turn index. Rules:
//   - Turn entries: copied iff their own index ≤ cutTurn.
//   - Non-turn entries with a turn anchor: copied iff the anchor's
//     index ≤ cutTurn. The MAX index across links is taken
//     (defensive against entries that pin multiple turns).
//   - Non-turn entries without a turn anchor: copied unconditionally.
func keepForCut(e memory.Entry, cutTurn int) (bool, error) {
	if e.Kind == turn.KindName {
		idx, _, err := parseTurnEntryID(e.ID)
		if err != nil {
			return false, err
		}
		return idx <= cutTurn, nil
	}
	maxAnchor := -1
	hasAnchor := false
	for _, l := range e.Links {
		if l.Kind != turn.KindName {
			continue
		}
		idx, _, err := parseTurnEntryID(l.ID)
		if err != nil {
			continue // malformed link target; skip
		}
		hasAnchor = true
		if idx > maxAnchor {
			maxAnchor = idx
		}
	}
	if !hasAnchor {
		return true, nil
	}
	return maxAnchor <= cutTurn, nil
}

// parseTurnEntryID parses a turn Entry.ID into (index, role).
// The format is fixed by turn.EntryID: "turn-NNNNNN-<role>".
func parseTurnEntryID(id string) (int, string, error) {
	if !strings.HasPrefix(id, turn.IDPrefix) {
		return 0, "", fmt.Errorf("memcopy: not a turn ID: %q", id)
	}
	body := strings.TrimPrefix(id, turn.IDPrefix)
	dash := strings.IndexByte(body, '-')
	if dash < 0 {
		return 0, "", fmt.Errorf("memcopy: malformed turn ID: %q", id)
	}
	var idx int
	if _, err := fmt.Sscanf(body[:dash], "%d", &idx); err != nil {
		return 0, "", fmt.Errorf("memcopy: parse turn index from %q: %w", id, err)
	}
	return idx, body[dash+1:], nil
}
