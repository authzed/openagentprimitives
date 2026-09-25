package sessionscope

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// stableID is the one-and-only session_scope entry ID per session.
const stableID = "scp-config"

// Put writes (or overwrites) the session_scope entry for scopeRef.
// Idempotent on identical content. Callers responsible for any
// optimistic-concurrency logic at a higher layer.
func Put(ctx context.Context, m memory.Memory, scopeRef memory.Scope, s scope.Scope) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("sessionscope.Put: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scopeRef,
		Kind:      Kind{}.Name(),
		ID:        stableID,
		CreatedAt: time.Now().UTC(),
		Content:   raw,
	})
	if err != nil {
		return fmt.Errorf("sessionscope.Put: %w", err)
	}
	return nil
}

// Get reads the session_scope entry. Returns (zero Scope, false, nil)
// when none has been written yet.
func Get(ctx context.Context, m memory.Memory, scopeRef memory.Scope) (scope.Scope, bool, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope: scopeRef,
		Kinds: []string{Kind{}.Name()},
	})
	if err != nil {
		return scope.Scope{}, false, fmt.Errorf("sessionscope.Get: query: %w", err)
	}
	if len(res.Entries) == 0 {
		return scope.Scope{}, false, nil
	}
	var s scope.Scope
	if err := json.Unmarshal(res.Entries[0].Content, &s); err != nil {
		return scope.Scope{}, false, fmt.Errorf("sessionscope.Get: unmarshal: %w", err)
	}
	return s, true, nil
}
