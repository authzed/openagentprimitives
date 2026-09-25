package infoleakagetaint

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/undecodable"
)

// Append writes one TaintRecord into the session's taint memory.
func Append(ctx context.Context, m memory.Memory, scope memory.Scope, rec TaintRecord) error {
	if rec.AccessedAt.IsZero() {
		rec.AccessedAt = time.Now().UTC()
	}
	content, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("infoleakagetaint.Append: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        memory.NewID(Kind{}),
		CreatedAt: rec.AccessedAt,
		Links: []memory.Link{
			{Relation: "for_tool_call", ID: rec.ToolUseID},
		},
		Content: content,
	})
	return err
}

// List returns all taint records for the scope. An entry that will not decode
// FAILS the whole read — deliberately, and unlike the record-shaped readers in
// sibling Kinds, which skip such an entry.
//
// The asymmetry is the point. A taint record is an INPUT TO A GATE: the
// respond-time audience check reads this list and denies when it errors
// (pkg/authz/hooks/infoleakaudience.go). Skipping an unreadable row would make
// the gate forget a taint, and forgetting a taint PERMITS the leak it was written
// to catch — the one degradation this list may not perform.
//
// The cost is accepted: the Kind is append-only, so an undecodable row can never
// be removed and the session's replies are denied from then on. That is denial of
// service rather than disclosure, the correct side to fail on; the log gives an
// operator the entry and the writer to act on, since deletion cannot.
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]TaintRecord, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope: scope,
		Kinds: []string{KindName},
	})
	if err != nil {
		return nil, fmt.Errorf("infoleakagetaint.List: %w", err)
	}
	out := make([]TaintRecord, 0, len(res.Entries))
	for _, e := range res.Entries {
		var r TaintRecord
		if err := json.Unmarshal(e.Content, &r); err != nil {
			// Logged as well as returned: the error reaches the caller as a
			// Deny reason, which names no entry and no writer, so without this
			// line the row that wedged the session is written down nowhere.
			undecodable.Refused("infoleakagetaint.List", scope, e, err)
			return nil, fmt.Errorf("infoleakagetaint.List: unmarshal %q: %w", e.ID, err)
		}
		out = append(out, r)
	}
	return out, nil
}
