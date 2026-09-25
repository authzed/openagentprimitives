package authzdecision

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/undecodable"
)

// Record persists one Decision. toolUseID is the LLM block id of the
// tool_use this Check evaluated against. It is recorded as a for_tool_call
// link with an empty Kind: a tool_use block id is NOT a memory entry id
// (it belongs to a "turn" entry but is not one), so claiming Kind:"turn"
// would fail Memory.Put's link-prefix validation. An empty Kind keeps the
// link unvalidated while preserving the id for later correlation.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, toolUseID string, d Decision) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("authzdecision.Record: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      Kind{}.Name(),
		ID:        memory.NewID(Kind{}),
		CreatedAt: time.Now().UTC(),
		Links: []memory.Link{
			{Relation: "for_tool_call", ID: toolUseID},
			{Relation: "for_resource", Kind: d.ResourceType, ID: d.ResourceID},
			{Relation: "by_subject", Kind: "user", ID: d.Subject},
		},
		Tags:    []string{"outcome:" + d.Outcome},
		Content: raw,
	})
	return err
}

// DeniesForResource returns every denied Decision in scope linked to
// (resKind, resID). Returns (nil, nil) when nothing matches.
func DeniesForResource(ctx context.Context, m memory.Memory, scope memory.Scope, resKind, resID string) ([]Decision, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope: scope, Kinds: []string{Kind{}.Name()},
		Tags:     []string{"outcome:" + OutcomeDenied},
		LinkedTo: []memory.LinkFilter{{Relation: "for_resource", Kind: resKind, ID: resID}},
	})
	if err != nil {
		return nil, err
	}
	out := make([]Decision, 0, len(res.Entries))
	for _, e := range res.Entries {
		var d Decision
		// A denial row that won't decode must NOT silently drop from the deny
		// set: this gates the asked-but-denied replay guard, so a corrupt or
		// tampered entry has to surface as a hard error rather than vanish
		// (which would re-open the very call it denied). Logged as well as
		// returned — the Kind is append-only, so the row can never be deleted
		// and this scope's deny lookup fails from now on; the only remedy is to
		// go fix the writer, whom the returned error does not name.
		if err := json.Unmarshal(e.Content, &d); err != nil {
			undecodable.Refused("authzdecision.DeniesForResource", scope, e, err)
			return nil, fmt.Errorf("authzdecision.DeniesForResource: decode %q: %w", e.ID, err)
		}
		out = append(out, d)
	}
	return out, nil
}
