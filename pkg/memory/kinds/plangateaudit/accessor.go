package plangateaudit

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/auditaccessor"
)

// Record appends one plan-gate entry. Unique ID per call.
//
// At is stamped here only when the caller left it zero. A caller that already
// built a record — the gate does — must pass its own At and reuse the same
// value on retry: the facade's append-only idempotency compares marshaled
// Content bytes, so re-stamping turns a safe retry into ErrAppendOnlyConflict.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	return auditaccessor.Append(ctx, m, scope, Kind{}, "plangateaudit.Record", c,
		func(c *Content) time.Time {
			if c.At.IsZero() {
				c.At = time.Now().UTC()
			}
			return c.At
		})
}

// List returns every plan-gate entry in scope (oldest-first).
//
// This is what the fold replays to rebuild approval state, spent budgets and
// denials after a runner restart, an idle-sleep re-hydrate, or a fork — which
// is why order matters and why the kind is append-only.
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]Content, error) {
	return auditaccessor.List[Content](ctx, m, scope, Kind{}, "plangateaudit.List")
}
