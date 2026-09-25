package preferencewrite

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/auditaccessor"
)

// Record appends one preference_write audit entry into scope — the USER scope
// of the acting user, never a session scope: this is a record of what the
// USER did to their own preferences, and belongs on their own tamper-evident
// chain. Every write is user-initiated and authenticated.
//
// m is deliberately auditaccessor.Putter, not memory.Memory: the caller
// hands this a write-only signing facade, and that facade has no reason to
// ever gain Query access to this audit trail just to append to it. A
// memory.Memory value (e.g. a bare *memory.Local in a test) still satisfies
// this — its method set is a superset.
//
// A unique ID per call (via memory.NewID), and CreatedAt stamped to now —
// c carries no timestamp field of its own, so every call to Record produces
// a genuinely new entry, including two back-to-back writes of the exact same
// preference value.
func Record(ctx context.Context, m auditaccessor.Putter, scope memory.Scope, c Content) error {
	return auditaccessor.Append(ctx, m, scope, Kind{}, "preferencewrite.Record", c,
		func(_ *Content) time.Time { return time.Now().UTC() })
}

// List returns every preference_write entry in scope, oldest-first.
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]Content, error) {
	return auditaccessor.List[Content](ctx, m, scope, Kind{}, "preferencewrite.List")
}
