package tool_dispatch_snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// entryID builds the deterministic ID. Zero-padded fields keep
// lexicographic order aligned with (turnIndex, sequence) for cheap
// range queries.
func entryID(c Content) string {
	return fmt.Sprintf("%s%06d-%03d-%s", IDPrefix, c.TurnIndex, c.Sequence, c.ToolUseID)
}

// Record writes one tool_dispatch_snapshot entry. Idempotent: a second
// Record with the same (TurnIndex, Sequence, ToolUseID) and identical
// payload is a no-op via Memory.Put's dedup. Links the entry to the
// turn via Relation="for_turn".
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("tool_dispatch_snapshot.Record: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        entryID(c),
		CreatedAt: time.Now().UTC(),
		Content:   raw,
		Links: []memory.Link{{
			Relation: "for_turn",
			Kind:     turn.KindName,
			ID:       turn.EntryID(c.TurnIndex, "assistant"),
		}},
	})
	return err
}

// ReadAll returns every dispatch-snapshot entry in scope, ascending
// (TurnIndex, Sequence).
func ReadAll(ctx context.Context, m memory.Memory, scope memory.Scope) ([]Content, error) {
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}})
	if err != nil {
		return nil, fmt.Errorf("tool_dispatch_snapshot.ReadAll: query: %w", err)
	}
	out := make([]Content, 0, len(res.Entries))
	for _, e := range res.Entries {
		var c Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			return nil, fmt.Errorf("tool_dispatch_snapshot.ReadAll: decode %q: %w", e.ID, err)
		}
		out = append(out, c)
	}
	sortEntries(out)
	return out, nil
}

// ForTurnRange returns dispatch-snapshot entries with TurnIndex strictly
// greater than `afterTurn` and at-or-below `throughTurn`, sorted ascending
// (TurnIndex, Sequence). The AgentSession restart reconciler calls this with
// afterTurn=cutTurnIndex and a large throughTurn.
func ForTurnRange(ctx context.Context, m memory.Memory, scope memory.Scope, afterTurn, throughTurn int) ([]Content, error) {
	all, err := ReadAll(ctx, m, scope)
	if err != nil {
		return nil, err
	}
	out := make([]Content, 0, len(all))
	for _, c := range all {
		if c.TurnIndex > afterTurn && c.TurnIndex <= throughTurn {
			out = append(out, c)
		}
	}
	return out, nil
}

func sortEntries(in []Content) {
	sort.Slice(in, func(i, j int) bool {
		if in[i].TurnIndex != in[j].TurnIndex {
			return in[i].TurnIndex < in[j].TurnIndex
		}
		return in[i].Sequence < in[j].Sequence
	})
}
