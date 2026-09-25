package uiviewmodel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Content is one stored slot rewrite.
type Content struct {
	// UI is the AgentUI CR name. The namespace is already the memory scope's
	// (scope.ID is "<ns>/<session>"), so it is not repeated here.
	UI string `json:"ui"`

	// Slot is the HOOK this record fills. The field and its json key predate
	// hooks — a hook is a slot with a position — and are kept so records
	// written before the tree existed still resolve.
	Slot string `json:"slot"`

	// Node is the fragment AS THE AGENT WROTE IT, verbatim. It is stored
	// unparsed on purpose: the platform vocabulary can gain a component
	// between the write and the read, and a record re-serialized through a
	// Go struct would quietly lose anything this build did not know about.
	// Every reader re-parses and RE-VALIDATES it (uicomponents.ResolveView),
	// so storing it verbatim widens nothing. Empty when Cleared — omitempty so
	// a cleared record round-trips as truly absent rather than a literal JSON
	// null a bare RawMessage tag would marshal a nil value as.
	Node json.RawMessage `json:"node,omitempty"`

	// Cleared marks an INTENTIONAL EMPTY: the agent collapsed this hook
	// (update_view clear: true). Node is empty when set. A cleared record is
	// a fill like any other — it replaces the author default and it is the
	// agent's — so it lives in the same record slot the fill would.
	Cleared bool `json:"cleared,omitempty"`

	// WrittenAt is when the agent wrote it. Presentation only — the record is
	// not part of any signed chain (see Kind.Retention).
	WrittenAt time.Time `json:"writtenAt"`
}

// EntryID is the deterministic memory Entry.ID for one (ui, slot): a
// hex-truncated SHA-256 of the two, joined by a separator that cannot occur
// in either half.
//
// Hashed rather than concatenated because a slot name carries NO character
// restriction — AgentUISlot.Name has no CRD pattern, and uicomponents.Validate
// requires only non-empty and unique. Concatenating author-chosen text would
// make the ID's charset an accident of the bundle, and would collide ("a",
// "b/c") with ("a/b", "c"). Content.Slot carries the readable name.
func EntryID(ui, slot string) string {
	sum := sha256.Sum256([]byte(ui + "\x00" + slot))
	return IDPrefix + hex.EncodeToString(sum[:])[:32]
}

// Record writes (or replaces) one slot's fragment. Errors are RETURNED, never
// swallowed: a lost write is a UI the agent believes it changed and did not.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("uiviewmodel.Record: marshal %s/%s: %w", c.UI, c.Slot, err)
	}
	if _, err := m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        EntryID(c.UI, c.Slot),
		CreatedAt: c.WrittenAt,
		Content:   raw,
	}); err != nil {
		return fmt.Errorf("uiviewmodel.Record: put %s/%s: %w", c.UI, c.Slot, err)
	}
	return nil
}

// List returns every stored fragment for one UI in scope, sorted by Slot so two
// calls in the same state produce the same order — a resolver whose rejection
// list reordered per request would be a log an operator cannot diff.
//
// The UI filter is applied HERE, after the Query, so no caller can forget it:
// one AgentSession can in principle be viewed through more than one AgentUI,
// and applying another UI's fragments would rewrite slots by name collision
// alone. Query.Limit is left unset before that filter, or another UI's records
// could exhaust it first.
func List(ctx context.Context, m memory.Memory, scope memory.Scope, ui string) ([]Content, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope: scope,
		Kinds: []string{KindName},
	})
	if err != nil {
		return nil, fmt.Errorf("uiviewmodel.List: query: %w", err)
	}
	out := make([]Content, 0, len(res.Entries))
	for _, e := range res.Entries {
		var c Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			// A record that will not decode is a UI that silently loses a slot —
			// returned, never skipped, per AGENTS.md's no-silent-errors rule.
			return nil, fmt.Errorf("uiviewmodel.List: unmarshal %q: %w", e.ID, err)
		}
		if c.UI != ui {
			continue
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Content) int { return strings.Compare(a.Slot, b.Slot) })
	return out, nil
}
