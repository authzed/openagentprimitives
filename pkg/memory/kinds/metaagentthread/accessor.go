package metaagentthread

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/auditaccessor"
)

// Append adds one message to the metaagent sub-thread. Unique ID per call.
func Append(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	return auditaccessor.Append(ctx, m, scope, Kind{}, "metaagentthread.Append", c,
		func(c *Content) time.Time {
			if c.Ts.IsZero() {
				c.Ts = time.Now().UTC()
			}
			return c.Ts
		})
}

// List returns every thread message in scope (oldest-first).
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]Content, error) {
	return auditaccessor.List[Content](ctx, m, scope, Kind{}, "metaagentthread.List")
}
