package sessionscope

import (
	"context"
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// ErrVersionConflict reports that the scope changed between the caller's read
// and its write, so the write was computed from a superseded document and was
// refused.
var ErrVersionConflict = errors.New("sessionscope: scope changed since it was read")

// PutIfVersion writes s only if the stored scope is still at expected.
//
// session_scope is read-modify-written by TWO independent writers: authzd,
// applying a human's scope decision, and the runner, appending bound slots on
// every dispatch round once any observed fact exists. Put replaces the whole
// document and memory.Entry carries no version, so without this the later
// write silently discarded whatever the other had just applied.
//
// The direction is what makes it a security defect rather than a wart:
// Disallow is the sole hard-deny enforcement surface, so the lost write can be
// a HardDeny the owner just clicked. It disappears with no error, and the
// scope_audit record still says it was applied — so the deny reads as enforced
// while nothing enforces it. The runner's write is frequent and its timing is
// not something the losing party controls.
//
// expected==0 means "no scope written yet" and admits the first write.
//
// This is compare-and-swap over a read, not an atomic backend primitive: the
// facade offers none, and adding one for a single Kind would be a larger
// change than the defect warrants. It closes the wide window (two operator
// round-trips) and leaves a narrow one. A caller that must not lose should
// retry on ErrVersionConflict, which is why the error is distinguishable.
func PutIfVersion(ctx context.Context, m memory.Memory, scopeRef memory.Scope, s scope.Scope, expected int64) error {
	cur, found, err := Get(ctx, m, scopeRef)
	if err != nil {
		return fmt.Errorf("sessionscope.PutIfVersion: read current: %w", err)
	}
	if !found {
		if expected != 0 {
			return fmt.Errorf("%w: expected version %d but no scope exists", ErrVersionConflict, expected)
		}
		return Put(ctx, m, scopeRef, s)
	}
	if cur.ScopeVersion != expected {
		return fmt.Errorf("%w: expected version %d, found %d", ErrVersionConflict, expected, cur.ScopeVersion)
	}
	return Put(ctx, m, scopeRef, s)
}
