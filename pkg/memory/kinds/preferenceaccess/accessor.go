package preferenceaccess

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/auditaccessor"
)

// Record appends one preference_access audit entry into scope — the
// SESSION scope the ?user-ref= request was made against, never the
// resolved subject's own user scope: this is a record of what the AGENT
// did, and belongs on the agent's own tamper-evident chain regardless of
// whether the reference resolved.
//
// m is deliberately auditaccessor.Putter, not memory.Memory: the caller
// (httpsrv.WithPreferenceAudit) hands this a write-only signing facade, and
// that facade has no reason to ever gain Query access to this audit trail
// just to append to it. A memory.Memory value (e.g. a bare *memory.Local in
// a test) still satisfies this — its method set is a superset.
//
// A unique ID per call (via memory.NewID), and CreatedAt stamped to now —
// c carries no timestamp field of its own (see Content's doc), so every
// call to Record produces a genuinely new entry, including two
// back-to-back reads of the exact same ref.
func Record(ctx context.Context, m auditaccessor.Putter, scope memory.Scope, c Content) error {
	return auditaccessor.Append(ctx, m, scope, Kind{}, "preferenceaccess.Record", c,
		func(_ *Content) time.Time { return time.Now().UTC() })
}

// List returns every preference_access entry in scope, oldest-first.
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]Content, error) {
	return auditaccessor.List[Content](ctx, m, scope, Kind{}, "preferenceaccess.List")
}
