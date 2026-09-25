// Package auditaccessor holds the shared append-one / list-all accessor
// boilerplate for the simple per-scope audit Kinds. Each caller supplies the four
// things that vary: its Content type, its memory.Kind, a label for error/log
// strings, and which of its own timestamp fields carries the entry time.
//
// Nothing about serialization, Kind name, scope, or retention lives here — it all
// flows in from the calling Kind.
package auditaccessor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/undecodable"
)

// Putter is the narrow write capability Append actually needs — just Put.
// Typed separately from memory.Memory (which every existing caller still
// passes without change, since its method set is a superset of this one) so
// a caller whose collaborator can write but not read — e.g. an HTTP handler
// holding a component's signing facade that must never gain Query access to
// an audit trail it only appends to — can call Append too.
type Putter interface {
	Put(ctx context.Context, e memory.Entry) (memory.Entry, error)
}

// Append marshals c and writes one append-only Entry for kind k into scope.
//
// label prefixes wrapped errors, so the message reads e.g. "scopeaudit.Record:
// …". ensureTS is called with a pointer to c before marshalling, so the caller
// can default the Kind's own timestamp field — the one field whose name varies
// across these Kinds (Ts / At / AppliedAt) — and returns the value to stamp on
// Entry.CreatedAt. tags is optional and nil-safe; it lands on Entry.Tags.
func Append[C any](
	ctx context.Context,
	m Putter,
	scope memory.Scope,
	k memory.Kind,
	label string,
	c C,
	ensureTS func(c *C) time.Time,
	tags ...string,
) error {
	createdAt := ensureTS(&c)
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("%s: marshal: %w", label, err)
	}
	if _, err := m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      k.Name(),
		ID:        memory.NewID(k),
		CreatedAt: createdAt,
		Tags:      tags,
		Content:   raw,
	}); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// List returns every entry of kind k in scope, oldest-first, decoded into C.
//
// An append-only row that fails to decode is logged at INFO with its publisher —
// the only handle an operator has on a row that cannot be deleted — and skipped,
// never silently dropped: a corrupt or tampered row vanishing from the result is
// precisely what the tamper-evident subsystem must surface. These Kinds are
// records, not gate inputs; see kinds/internal/undecodable for when skipping is
// the wrong disposition.
func List[C any](
	ctx context.Context,
	m memory.Memory,
	scope memory.Scope,
	k memory.Kind,
	label string,
) ([]C, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope:   scope,
		Kinds:   []string{k.Name()},
		OrderBy: memory.OrderBy{Field: "createdAt"}, // oldest-first; backends iterate maps in random order otherwise
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	out := make([]C, 0, len(res.Entries))
	for _, e := range res.Entries {
		var c C
		if err := json.Unmarshal(e.Content, &c); err != nil {
			undecodable.Skipped(label, scope, e, err)
			continue
		}
		out = append(out, c)
	}
	return out, nil
}
