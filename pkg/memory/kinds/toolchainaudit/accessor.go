package toolchainaudit

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/auditaccessor"
)

// Record appends one audit entry. The Memory passed in MUST be the caller's
// provenance-signing facade (provenance.NewSigningMemory); the append-only
// facade rejects unsigned writes, so an unsigned Memory here is a hard error at
// Put, not a silent unsigned entry.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	return auditaccessor.Append(ctx, m, scope, Kind{}, "toolchainaudit.Record", c,
		func(c *Content) time.Time {
			if c.At.IsZero() {
				c.At = time.Now().UTC()
			}
			return c.At
		})
}

// List returns every audit entry in scope (oldest-first).
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]Content, error) {
	return auditaccessor.List[Content](ctx, m, scope, Kind{}, "toolchainaudit.List")
}
