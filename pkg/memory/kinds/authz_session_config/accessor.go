package authz_session_config

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

const stableConfigID = "asc-config"

// Snapshot writes the per-session config. Idempotent: a second call
// with different content replaces the prior entry (stable ID).
func Snapshot(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("authz_session_config.Snapshot: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      Kind{}.Name(),
		ID:        stableConfigID,
		CreatedAt: time.Now().UTC(),
		Content:   raw,
	})
	return err
}

func Get(ctx context.Context, m memory.Memory, scope memory.Scope) (Content, bool, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope: scope, Kinds: []string{Kind{}.Name()},
	})
	if err != nil {
		return Content{}, false, err
	}
	if len(res.Entries) == 0 {
		return Content{}, false, nil
	}
	var c Content
	if err := json.Unmarshal(res.Entries[0].Content, &c); err != nil {
		return Content{}, false, err
	}
	return c, true, nil
}
