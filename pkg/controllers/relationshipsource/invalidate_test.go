package relationshipsource

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// -----------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------

// newInvalidator builds an Invalidator over a fixed fixture Source — every
// test in this file cares only about ONE RelationshipSource's dirty set, so
// the identity of that source is never itself under test.
func newInvalidator(t *testing.T, opts ...invalidatorOption) *Invalidator {
	t.Helper()
	return NewInvalidator(types.NamespacedName{Namespace: "ns", Name: "src"}, opts...)
}

// membershipEvent builds a MembershipEvent naming scope — standing in for
// whatever a future NATS subscriber decodes off the bus for a Slack
// member_joined_channel/member_left_channel notification.
func membershipEvent(scope string) MembershipEvent {
	return MembershipEvent{Scope: relsync.ScopeID(scope)}
}

// waitFor polls fn every 10ms until it returns true or d elapses, failing
// the test on timeout. Mirrors pkg/controllers/agentsession's eventually
// helper (same shape, named per this task's brief).
func waitFor(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition never became true within %v", d)
}

// -----------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------

// The event NEVER writes a tuple. It marks a scope dirty and wakes a
// reconcile, which re-fetches AFTER the event — so the delta is in the
// snapshot by construction and the prune-versus-delta race cannot occur.
//
// OnWake is wired to write directly, deliberately: it stands in for what a
// real reconcile eventually does, so that if Handle ever called OnWake
// synchronously (or wrote itself, on any path) the write would show up in w
// before this test's assertions run. A debounce of an hour means the timer
// cannot fire on its own during the test, so the ONLY way w sees a write is
// a bug in Handle itself.
func TestInvalidate_EventWritesNoTuples(t *testing.T) {
	ctx := context.Background()
	w := &fakeRelWriter{}
	inv := newInvalidator(t, withDebounce(time.Hour))
	inv.OnWake = func(types.NamespacedName) {
		_, _ = w.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{})
		_, _ = w.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{})
	}

	require.NoError(t, inv.Handle(ctx, membershipEvent("C0193FQ2X")))

	assert.Empty(t, w.writes, "an event must never write; the reconcile does")
	assert.Empty(t, w.deletes)
}

// Every Slack app in a channel gets its own copy of a membership event, so
// N per-agent apps mean N copies. Marking dirty is idempotent and the
// debounce coalesces: N duplicates, one reconcile.
func TestInvalidate_DuplicateEventsFromNAppsProduceOneReconcile(t *testing.T) {
	ctx := context.Background()
	inv := newInvalidator(t, withDebounce(50*time.Millisecond))
	var wakes int32
	inv.OnWake = func(types.NamespacedName) { atomic.AddInt32(&wakes, 1) }

	for i := 0; i < 5; i++ { // five apps, five copies of one event
		require.NoError(t, inv.Handle(ctx, membershipEvent("C0193FQ2X")))
	}
	waitFor(t, 500*time.Millisecond, func() bool { return atomic.LoadInt32(&wakes) > 0 })

	assert.Equal(t, int32(1), atomic.LoadInt32(&wakes),
		"five duplicate events must coalesce into one reconcile")
	assert.ElementsMatch(t, []relsync.ScopeID{"C0193FQ2X"}, inv.DirtyScopes())
}

// The previous test proves N copies of the SAME scope coalesce; this proves
// the more interesting case — several DISTINCT scopes, each arriving once,
// inside one debounce window. Invalidator shares a single timer across every
// scope for one Source (see Handle's own doc), so a burst of unrelated joins
// across several channels still wakes the reconcile once, which then sees
// every one of them via DirtyScopes. A future per-scope-timer refactor (one
// timer keyed by scope, rather than one shared by Source) would silently
// turn this into N wakes without TestInvalidate_DuplicateEventsFromNAppsProduceOneReconcile
// ever noticing, since that test never varies the scope.
func TestInvalidate_SeveralDistinctScopesInOneWindowProduceOneReconcile(t *testing.T) {
	ctx := context.Background()
	inv := newInvalidator(t, withDebounce(50*time.Millisecond))
	var wakes int32
	inv.OnWake = func(types.NamespacedName) { atomic.AddInt32(&wakes, 1) }

	for _, scope := range []string{"C1", "C2", "C3"} { // three distinct channels
		require.NoError(t, inv.Handle(ctx, membershipEvent(scope)))
	}
	waitFor(t, 500*time.Millisecond, func() bool { return atomic.LoadInt32(&wakes) > 0 })

	assert.Equal(t, int32(1), atomic.LoadInt32(&wakes),
		"three distinct scopes inside one debounce window must still coalesce into one reconcile")
	assert.ElementsMatch(t, []relsync.ScopeID{"C1", "C2", "C3"}, inv.DirtyScopes(),
		"the single reconcile must see every scope the window collected, not just the last one")
}

// A scoped wake never reaps: a scope cannot be shown absent without
// enumerating, so a pass that fetched only the dirty scopes must not delete
// anything belonging to the scopes it never looked at. Proved here directly
// against relsync.Pass (already the case since Task 4 — see
// pkg/platform/relsync/sync_test.go's own
// TestPass_ScopedPassNeverReapsEvenWithACompleteEnumeration) using THIS
// package's own SpiceDBClient-shaped fixtures, since this is the package
// that will actually call Pass with OnlyScopes once a reconcile is woken.
func TestInvalidate_ScopedPassDoesNotReap(t *testing.T) {
	ctx := context.Background()
	k := &fakeKind{
		pages:   []relsync.ScopePage{{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}}, Complete: true}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": {tup("C1", "alice")}},
	}
	w := &fakeRelWriter{}
	r := &fakeReader{owned: []spicedb.Tuple{tup("C9", "bob")}} // a scope not in this pass

	res, err := relsync.Pass(ctx, relsync.PassInput{
		Kind: k, Writer: w, Reader: r, OnlyScopes: []relsync.ScopeID{"C1"},
	})

	require.NoError(t, err)
	assert.Equal(t, 0, res.ReapedScopes)
	assert.Empty(t, w.deletes, "a scoped pass must delete nothing outside its scopes")
}

// An event naming no scope is recorded (returned, and logged) rather than
// silently accepted as a no-op — AGENTS.md's no-silent-errors rule applies
// to this package's own event path exactly as much as to a reconcile
// failure.
func TestInvalidate_EventWithNoScopeIsRecordedNotDropped(t *testing.T) {
	ctx := context.Background()
	inv := newInvalidator(t)

	err := inv.Handle(ctx, MembershipEvent{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "names no scope")
	assert.Empty(t, inv.DirtyScopes(), "a refused event must not leave a dirty entry behind")
}

// An Invalidator with no Source configured is a wiring bug, not a valid
// "nothing to do here" — Handle must say so rather than quietly
// accumulating a dirty scope under a key no reconcile will ever be asked to
// look at.
func TestInvalidate_EventWithNoSourceConfiguredIsRecordedNotDropped(t *testing.T) {
	ctx := context.Background()
	inv := &Invalidator{} // zero value: no Source

	err := inv.Handle(ctx, membershipEvent("C1"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Source configured")
	assert.Empty(t, inv.DirtyScopes())
}
