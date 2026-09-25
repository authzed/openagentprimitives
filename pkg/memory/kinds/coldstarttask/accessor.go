package coldstarttask

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// stableID is the one-and-only cold_start_task entry ID per session.
const stableID = "cst-config"

// Put writes (overwrites) the session's single cold_start_task entry.
// Defaults DecidedAt to now when unset.
func Put(ctx context.Context, m memory.Memory, scopeRef memory.Scope, c Content) error {
	if c.DecidedAt.IsZero() {
		c.DecidedAt = time.Now().UTC()
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("coldstarttask.Put: marshal: %w", err)
	}
	if _, err := m.Put(ctx, memory.Entry{
		Scope:     scopeRef,
		Kind:      Kind{}.Name(),
		ID:        stableID,
		CreatedAt: c.DecidedAt,
		Content:   raw,
	}); err != nil {
		return fmt.Errorf("coldstarttask.Put: %w", err)
	}
	return nil
}

// Get reads the cold_start_task entry. Returns (zero Content, false, nil)
// when none has been written yet.
func Get(ctx context.Context, m memory.Memory, scopeRef memory.Scope) (Content, bool, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope: scopeRef,
		Kinds: []string{Kind{}.Name()},
	})
	if err != nil {
		return Content{}, false, fmt.Errorf("coldstarttask.Get: %w", err)
	}
	if len(res.Entries) == 0 {
		return Content{}, false, nil
	}
	var c Content
	if err := json.Unmarshal(res.Entries[0].Content, &c); err != nil {
		return Content{}, false, fmt.Errorf("coldstarttask.Get: unmarshal: %w", err)
	}
	return c, true, nil
}
