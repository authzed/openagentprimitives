package extraction_state

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func Record(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	if c.StartedAt.IsZero() {
		c.StartedAt = time.Now().UTC()
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("extraction_state.Record: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      Kind{}.Name(),
		ID:        stableID(c.TurnIndex),
		CreatedAt: c.StartedAt,
		Tags:      []string{"status:" + c.Status, "turn:" + strconv.Itoa(c.TurnIndex)},
		Content:   raw,
	})
	return err
}

func ForTurn(ctx context.Context, m memory.Memory, scope memory.Scope, turnIndex int) (Content, bool, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope: scope, Kinds: []string{Kind{}.Name()},
		Tags: []string{"turn:" + strconv.Itoa(turnIndex)},
		// Content key, not the Go field name: backends render the predicate as
		// content->>'<path>' against the stored JSON.
		FieldEquals: []memory.FieldFilter{
			{Path: "turnIndex", Value: turnIndex},
		},
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

func stableID(turnIndex int) string {
	return "exs-" + strconv.Itoa(turnIndex)
}
