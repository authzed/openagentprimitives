package lineage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// RecordFork writes both lineage edges in one logical operation. Idempotent:
// re-recording the same fork is a no-op (Memory.Put dedups identical content).
// The two Puts are NOT atomic — an error from the second leaves the out-edge
// written and the in-edge missing. The caller is the restart reconciler, so a
// later reconcile re-runs this and repairs the half-written pair.
func RecordFork(
	ctx context.Context,
	mem memory.Memory,
	parentScope memory.Scope, parentName string,
	childScope memory.Scope, childName string,
	cutTurn int,
	reason string,
) error {
	now := time.Now().UTC()

	outID := IDPrefixForkOut + childName
	outContent := Content{Direction: "out", Peer: childName, AtTurn: cutTurn, Reason: reason}
	outRaw, err := json.Marshal(outContent)
	if err != nil {
		return fmt.Errorf("lineage.RecordFork: marshal out: %w", err)
	}

	inID := IDPrefixForkIn + parentName
	inContent := Content{Direction: "in", Peer: parentName, AtTurn: cutTurn, Reason: reason}
	inRaw, err := json.Marshal(inContent)
	if err != nil {
		return fmt.Errorf("lineage.RecordFork: marshal in: %w", err)
	}

	if _, err := mem.Put(ctx, memory.Entry{
		Scope: parentScope, Kind: KindName, ID: outID,
		CreatedAt: now, Content: outRaw,
	}); err != nil {
		return fmt.Errorf("lineage.RecordFork: put out-edge: %w", err)
	}
	if _, err := mem.Put(ctx, memory.Entry{
		Scope: childScope, Kind: KindName, ID: inID,
		CreatedAt: now, Content: inRaw,
	}); err != nil {
		return fmt.Errorf("lineage.RecordFork: put in-edge: %w", err)
	}
	return nil
}

// OutEdges returns this scope's fork-out edges (children), sorted by
// Peer name for deterministic iteration.
func OutEdges(ctx context.Context, mem memory.Memory, scope memory.Scope) ([]Content, error) {
	return edgesWithPrefix(ctx, mem, scope, IDPrefixForkOut)
}

// InEdges returns this scope's fork-in edges (parents), sorted by Peer name.
// A session normally has exactly one; more than one is a merge-style shape
// nothing in the fork paths produces today.
func InEdges(ctx context.Context, mem memory.Memory, scope memory.Scope) ([]Content, error) {
	return edgesWithPrefix(ctx, mem, scope, IDPrefixForkIn)
}

func edgesWithPrefix(ctx context.Context, mem memory.Memory, scope memory.Scope, prefix string) ([]Content, error) {
	res, err := mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}})
	if err != nil {
		return nil, fmt.Errorf("lineage.edgesWithPrefix(%q): query: %w", prefix, err)
	}
	out := make([]Content, 0, len(res.Entries))
	for _, e := range res.Entries {
		if !strings.HasPrefix(e.ID, prefix) {
			continue
		}
		var c Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			return nil, fmt.Errorf("lineage.edgesWithPrefix: decode %q: %w", e.ID, err)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Peer < out[j].Peer })
	return out, nil
}
