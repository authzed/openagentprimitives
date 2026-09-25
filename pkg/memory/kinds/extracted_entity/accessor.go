package extracted_entity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func Record(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	if c.ExtractedAt.IsZero() {
		c.ExtractedAt = time.Now().UTC()
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("extracted_entity.Record: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      Kind{}.Name(),
		ID:        stableID(c.TurnIndex, c.ResourceType, c.ResourceID),
		CreatedAt: c.ExtractedAt,
		Links: []memory.Link{
			{Relation: "for_resource", Kind: c.ResourceType, ID: c.ResourceID},
		},
		Tags:    []string{"turn:" + strconv.Itoa(c.TurnIndex)},
		Content: raw,
	})
	return err
}

func ForTurn(ctx context.Context, m memory.Memory, scope memory.Scope, turnIndex int) ([]Content, error) {
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
		return nil, err
	}
	out := make([]Content, 0, len(res.Entries))
	for _, e := range res.Entries {
		var c Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			// Append-only audit entry that won't decode: log loudly rather
			// than silently drop it — a corrupt/tampered row vanishing from
			// the read result is exactly what the tamper-evident subsystem
			// must surface.
			slog.Info("extracted_entity.ForTurn: skipping undecodable entry", "scope", scope.ID, "entry", e.ID, "err", err.Error())
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func stableID(turnIndex int, resourceType, resourceID string) string {
	sum := sha256.Sum256([]byte(strconv.Itoa(turnIndex) + "\x00" + resourceType + "\x00" + resourceID))
	return "eex-" + strconv.Itoa(turnIndex) + "-" + hex.EncodeToString(sum[:6])
}
