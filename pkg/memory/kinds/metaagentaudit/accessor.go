package metaagentaudit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/auditaccessor"
)

// Record appends one metaagent audit entry. Unique ID per call.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	return auditaccessor.Append(ctx, m, scope, Kind{}, "metaagentaudit.Record", c,
		func(c *Content) time.Time {
			if c.Ts.IsZero() {
				c.Ts = time.Now().UTC()
			}
			return c.Ts
		})
}

// List returns every metaagent audit entry in scope (oldest-first).
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]Content, error) {
	return auditaccessor.List[Content](ctx, m, scope, Kind{}, "metaagentaudit.List")
}

// requestIDTagPrefix mirrors pkg/memory/kinds/approval: request-time
// entries are tagged with the channel-visible request id.
const requestIDTagPrefix = "request:"

// RecordRequested persists the request-time record for a scope-approval,
// written by authzd when it publishes the metaagent_scope_approval
// payload. Tagged kind:request + request:<id> for RequestByID; decision
// records (Record) stay untagged and unchanged.
func RecordRequested(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	if c.RequestID == "" {
		return fmt.Errorf("metaagentaudit.RecordRequested: RequestID is required")
	}
	return auditaccessor.Append(ctx, m, scope, Kind{}, "metaagentaudit.RecordRequested", c,
		func(c *Content) time.Time {
			if c.Ts.IsZero() {
				c.Ts = time.Now().UTC()
			}
			return c.Ts
		},
		"kind:request", requestIDTagPrefix+c.RequestID)
}

// RequestByID returns the request-time record tagged request:<requestID>,
// or (nil, nil) when absent (requests predating persistence).
func RequestByID(ctx context.Context, m memory.Memory, scope memory.Scope, requestID string) (*Content, error) {
	if requestID == "" {
		return nil, nil
	}
	res, err := m.Query(ctx, memory.Query{
		Scope: scope, Kinds: []string{Kind{}.Name()},
		Tags: []string{"kind:request", requestIDTagPrefix + requestID},
	})
	if err != nil {
		return nil, fmt.Errorf("metaagentaudit.RequestByID: %w", err)
	}
	for _, e := range res.Entries {
		var c Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			return nil, fmt.Errorf("metaagentaudit.RequestByID: decode %s: %w", e.ID, err)
		}
		return &c, nil
	}
	return nil, nil
}
