// Package relwrites — the production SlotBoundChecker.
//
// NewSlotBoundChecker turns a session's slot-grant set into the per-tuple
// answer Run needs: "is the agent allowed to write onto THIS instance?"
package relwrites

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// SlotGrantLister is the read surface NewSlotBoundChecker needs: every
// instance bound into a slot for one session. (*spicedb.Client) satisfies it
// via ListSlotGrants.
//
// Declared here as a minimal subset rather than taking the concrete client —
// the same pattern Writer follows for the write side — so this package keeps
// its one-way dependency on pkg/authz and tests can substitute a recorder
// without a SpiceDB container.
type SlotGrantLister interface {
	ListSlotGrants(ctx context.Context, ns, name string) ([]authz.SlotBinding, error)
}

// NewSlotBoundChecker returns the checker Run consults for a block whose
// RequireSlotBound is true: the tuple's resource is slot-bound iff the session
// ns/name holds ANY slot grant on it.
//
// The grant's PERMISSION is deliberately ignored. A slot grant is a human act
// naming an INSTANCE — which permission it carries is the pool machinery's
// concern (see authz.SlotGrantRelationName for why the relation is
// per-permission), not this gate's. This gate asks a narrower question than
// the permission Check that runs before the tool call: not "may the agent do
// this?" but "is this instance one the session was bound to at all?".
//
// Not cached, by construction. ListSlotGrants reads FullyConsistent because it
// decides what the agent may reach; memoizing the answer here would hand back
// authority a human revoked mid-session, which is exactly what the consistency
// choice one layer down exists to prevent. A tool call is not a hot path.
//
// A nil lister yields a NIL checker, not a permissive one: that is precisely
// the unwired case Run refuses loudly, per block, by name. A runner that could
// not reach SpiceDB must not be a runner where a marked block writes
// unchecked — see Run's doc comment.
func NewSlotBoundChecker(lister SlotGrantLister, ns, name string) SlotBoundChecker {
	if lister == nil {
		return nil
	}
	return func(ctx context.Context, resource string) (bool, error) {
		held, err := lister.ListSlotGrants(ctx, ns, name)
		if err != nil {
			return false, fmt.Errorf("list slot grants for %s/%s: %w", ns, name, err)
		}
		for _, b := range held {
			// Compared as the whole "<type>:<id>" reference the tuple carries.
			// Matching either half alone would bind a neighbouring instance —
			// a different pull request, or an issue that happens to share an
			// id with a granted PR.
			if b.ResourceType+":"+b.ResourceID.String() == resource {
				return true, nil
			}
		}
		return false, nil
	}
}
