// Package pools answers one question: which resource memory pools may this
// session touch, and in which direction?
//
// It reads SpiceDB and returns memory scopes (package memory, for Scope and
// ResourceScope). It deliberately does NOT import the memory facade — the
// facade's authorizer consults this package, and nothing in package memory
// imports pools back, so there is no cycle between the two. A cycle would
// make either untestable in isolation.
package pools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// writeMemoryPermission is the slot permission that authorizes writing into a
// resource's pool. The spec requires the write capability to be named write_*
// precisely so it cannot be confused with the audience permission: ComposeSlots
// OWNS the permission line it composes, so a slot pointed at view_memory would
// silently replace the audience definition with the slot expression.
const writeMemoryPermission = "write_memory"

// RelFilter is the subject-scoped relationship read this package needs.
type RelFilter struct {
	SubjectType string
	SubjectID   string
}

// RelationshipReader is the SpiceDB read this package depends on, narrowed to
// what it uses so a test can supply a fake without a container.
type RelationshipReader interface {
	ReadRelationships(ctx context.Context, f RelFilter) ([]authz.Relation, error)
}

// Pools are one session's resource memory pools, split by direction.
type Pools struct {
	// Read pools: every resource the session holds ANY slot on. Reading a pool
	// is disclosure its audience already permits.
	Read []memory.Scope
	// Write pools: only those held under a write_memory slot. Writing is an
	// egress into someone else's pool, so it needs its own grant.
	Write []memory.Scope
}

// ForSession returns the pools the session may touch.
//
// A slot grant is <type>:<id>#slot_grant_<perm>@agentsession:<ns>/<name>, and
// the relation is PER-PERMISSION. So the read/write asymmetry the spec calls
// for is readable directly off the tuples: any slot_grant_* makes a read pool,
// slot_grant_write_memory makes a write pool. Nothing tracks it separately, so
// nothing can drift out of agreement with what was actually granted.
//
// Results are sorted. They become minted approvals and pt-tags, both of which
// must be a pure function of the grant set rather than of SpiceDB's row order.
func ForSession(ctx context.Context, r RelationshipReader, ns, name string) (Pools, error) {
	rels, err := r.ReadRelationships(ctx, RelFilter{
		SubjectType: "agentsession",
		SubjectID:   ns + "/" + name,
	})
	if err != nil {
		return Pools{}, fmt.Errorf("pools: read slot grants for %s/%s: %w", ns, name, err)
	}

	writePrefix := authz.SlotGrantRelationName(writeMemoryPermission)
	readSet := map[string]memory.Scope{}
	writeSet := map[string]memory.Scope{}

	for _, rel := range rels {
		if !strings.HasPrefix(rel.Relation, authz.SlotGrantRelationPrefix) {
			continue
		}
		scope, err := memory.ResourceScope(rel.ResourceType, rel.ResourceID)
		if err != nil {
			// A grant naming an object this package cannot address is a real
			// finding, not a row to skip silently: it means something wrote a
			// tuple whose object ref cannot become a scope.
			return Pools{}, fmt.Errorf("pools: slot grant on %s:%s is not addressable as a memory scope: %w",
				rel.ResourceType, rel.ResourceID, err)
		}
		readSet[scope.ID] = scope
		if rel.Relation == writePrefix {
			writeSet[scope.ID] = scope
		}
	}

	return Pools{Read: sortedScopes(readSet), Write: sortedScopes(writeSet)}, nil
}

func sortedScopes(m map[string]memory.Scope) []memory.Scope {
	if len(m) == 0 {
		return nil
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]memory.Scope, 0, len(ids))
	for _, id := range ids {
		out = append(out, m[id])
	}
	return out
}
