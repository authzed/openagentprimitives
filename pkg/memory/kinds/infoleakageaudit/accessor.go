package infoleakageaudit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/undecodable"
)

// Append writes one AuditRecord into the scope's audit memory.
func Append(ctx context.Context, m memory.Memory, scope memory.Scope, rec AuditRecord) error {
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	content, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("infoleakageaudit.Append: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        memory.NewID(Kind{}),
		CreatedAt: rec.At,
		Content:   content,
	})
	return err
}

// List returns all audit records for the scope. An entry that will not decode is
// skipped and logged: the Kind is append-only, so such a row can never be
// removed, and a hard error here would fail every read of the scope's leakage
// audit forever. Unlike the taint and decision Kinds, these records gate NOTHING
// — they are the trail of decisions already made elsewhere, so a missing row
// degrades the trail rather than re-opening anything. The log names the entry and
// its publisher: a gap in a tamper-evident trail must not be silent.
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]AuditRecord, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope: scope,
		Kinds: []string{KindName},
	})
	if err != nil {
		return nil, fmt.Errorf("infoleakageaudit.List: %w", err)
	}
	out := make([]AuditRecord, 0, len(res.Entries))
	for _, e := range res.Entries {
		var r AuditRecord
		if err := json.Unmarshal(e.Content, &r); err != nil {
			undecodable.Skipped("infoleakageaudit.List", scope, e, err)
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
