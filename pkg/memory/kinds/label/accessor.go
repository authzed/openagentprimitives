package label

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Record stamps (resourceType, id) → label. Re-Record overwrites the
// existing entry (key collision is by deterministic ID — same
// (resourceType, id) produces the same entry ID).
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, resourceType, id, lbl string) error {
	if resourceType == "" || id == "" || lbl == "" {
		return nil
	}
	raw, err := json.Marshal(Label{ResourceType: resourceType, ResourceID: id, Label: lbl})
	if err != nil {
		return fmt.Errorf("label.Record: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      Kind{}.Name(),
		ID:        deterministicID(resourceType, id),
		CreatedAt: time.Now().UTC(),
		Tags:      []string{"trust:untrusted"},
		Content:   raw,
	})
	return err
}

// Get returns the label for (resourceType, id) and ok=true on hit;
// ("", false, nil) on miss. The lookup is a single-ID Query — the
// Memory interface exposes Query, not a point Get, so the
// deterministic entry ID is used as the IDs filter.
func Get(ctx context.Context, m memory.Memory, scope memory.Scope, resourceType, id string) (string, bool, error) {
	if resourceType == "" || id == "" {
		return "", false, nil
	}
	res, err := m.Query(ctx, memory.Query{
		Scope: scope,
		Kinds: []string{Kind{}.Name()},
		IDs:   []string{deterministicID(resourceType, id)},
	})
	if err != nil {
		return "", false, err
	}
	if len(res.Entries) == 0 {
		return "", false, nil
	}
	var l Label
	if err := json.Unmarshal(res.Entries[0].Content, &l); err != nil {
		return "", false, err
	}
	return l.Label, true, nil
}

func deterministicID(resourceType, id string) string {
	return Kind{}.IDPrefix() + sanitize(resourceType) + "-" + sanitize(id)
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ":", "-")
	return s
}
