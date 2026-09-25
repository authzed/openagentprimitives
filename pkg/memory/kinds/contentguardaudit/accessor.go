package contentguardaudit

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/auditaccessor"
)

// Record appends one audit entry. Unique ID per call.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	return auditaccessor.Append(ctx, m, scope, Kind{}, "contentguardaudit.Record", c,
		func(c *Content) time.Time {
			if c.At.IsZero() {
				c.At = time.Now().UTC()
			}
			return c.At
		})
}

// List returns every audit entry in scope (oldest-first).
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]Content, error) {
	return auditaccessor.List[Content](ctx, m, scope, Kind{}, "contentguardaudit.List")
}
