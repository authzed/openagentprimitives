package infoleakagedecision

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/undecodable"
)

// Record writes one DecisionRecord into the session's leakage-decision memory.
// decision must be DecisionApproved or DecisionDenied. Re-recording the same
// tuple is harmless (the readers fold over all rows).
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, decision, resourceType, resourceID string) error {
	rec := DecisionRecord{
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Decision:     decision,
		At:           time.Now().UTC(),
	}
	content, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("infoleakagedecision.Record: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        memory.NewID(Kind{}),
		CreatedAt: rec.At,
		Content:   content,
	})
	if err != nil {
		return fmt.Errorf("infoleakagedecision.Record: %w", err)
	}
	return nil
}

// List returns all decision records for the scope.
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]DecisionRecord, error) {
	return records(ctx, m, scope, nil)
}

// records reads the scope's decision entries, narrowed by fields where the
// backend can do it. Content keys, not Go field names — the SQL backends
// render each predicate as content->>'<path>'.
//
// An entry that will not decode FAILS the read rather than being skipped — the
// same call infoleakagetaint.List makes, for the same reason: these rows are gate
// inputs, not a record. Dropping one makes IsApproved/IsDenied answer from an
// incomplete set, a recorded decision silently ceasing to exist, so the read
// fails closed instead. The permanence of that is accepted (append-only; the row
// cannot be deleted), which is why the entry and its publisher are logged — the
// error travels to the caller as a verdict and names neither.
func records(ctx context.Context, m memory.Memory, scope memory.Scope, fields []memory.FieldFilter) ([]DecisionRecord, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope:       scope,
		Kinds:       []string{KindName},
		FieldEquals: fields,
	})
	if err != nil {
		return nil, fmt.Errorf("infoleakagedecision.List: %w", err)
	}
	out := make([]DecisionRecord, 0, len(res.Entries))
	for _, e := range res.Entries {
		var r DecisionRecord
		if err := json.Unmarshal(e.Content, &r); err != nil {
			undecodable.Refused("infoleakagedecision.List", scope, e, err)
			return nil, fmt.Errorf("infoleakagedecision.List: unmarshal %q: %w", e.ID, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// IsApproved reports whether (resourceType, resourceID) has an approved decision
// recorded for the scope.
func IsApproved(ctx context.Context, m memory.Memory, scope memory.Scope, resourceType, resourceID string) (bool, error) {
	return hasDecision(ctx, m, scope, DecisionApproved, resourceType, resourceID)
}

// IsDenied reports whether (resourceType, resourceID) has a denied decision
// recorded for the scope.
func IsDenied(ctx context.Context, m memory.Memory, scope memory.Scope, resourceType, resourceID string) (bool, error) {
	return hasDecision(ctx, m, scope, DecisionDenied, resourceType, resourceID)
}

func hasDecision(ctx context.Context, m memory.Memory, scope memory.Scope, decision, resourceType, resourceID string) (bool, error) {
	// Ask for the one record rather than the session's whole decision
	// history: this runs once per tainted resource on every reply, over HTTP
	// from the runner, and the taint list only grows.
	//
	// No Limit, deliberately. A backend without ContentSchemas drops
	// FieldEquals and answers with the unfiltered kind — a Limit would then
	// truncate that unfiltered set and turn a recorded decision into a false
	// negative. The Go-side match below stays authoritative for exactly that
	// reason; the predicates are an optimization for backends that index
	// content, not a filter this function may rely on.
	recs, err := records(ctx, m, scope, []memory.FieldFilter{
		{Path: "resourceType", Value: resourceType},
		{Path: "resourceID", Value: resourceID},
		{Path: "decision", Value: decision},
	})
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		if r.Decision == decision && r.ResourceType == resourceType && r.ResourceID == resourceID {
			return true, nil
		}
	}
	return false, nil
}
