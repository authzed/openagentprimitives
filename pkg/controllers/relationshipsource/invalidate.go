// pkg/controllers/relationshipsource/invalidate.go
//
// An upstream event closes the staleness window between periodic passes, but
// it never writes a tuple itself:
//
//	Slack event → Socket Mode (the app's own long-lived connection,
//	              pkg/channels/channelkinds/slack/listener.go's events_api
//	              handling — this app has no request_url, so channelwebhook's
//	              signed-HTTP route is never in this path)
//	            → NATS
//	            → mark (source, scope) dirty
//	            → wake a debounced reconcile
//
// The reconcile that OnWake eventually triggers re-fetches the dirty scope
// and writes through the SAME diff-and-prune path a periodic pass uses
// (relsync.Pass, this time with PassInput.OnlyScopes set) — never a path of
// its own. That ordering is not stylistic: writing directly from the event
// would let the periodic pass's prune race it (a pass reading upstream at T0
// could diff against a snapshot that predates a T1 event-driven write and
// delete what the event just added). Fetching only AFTER the event —
// because the event triggered the fetch — puts the delta inside the
// snapshot by construction, so the race cannot occur.
//
// Everything upstream of Handle (an HTTP callback verified over
// channelwebhook, a NATS subscription matching an event to the
// RelationshipSource whose spec.kind it names) is a later task's wiring —
// this file is the seam that wiring calls into, the same way Task 5's
// RetryAfter interface was declared before any registered Kind implemented
// it.
package relationshipsource

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// defaultInvalidateDebounce is how long Handle waits after the LAST event
// before calling OnWake. Long enough to coalesce one membership change's
// fan-out — every Slack app installed in a channel gets its own copy of the
// same event (AGENTS.md's "Slack socket: 1 delivery per app") — short
// enough that a join/leave is reflected well inside the periodic sync
// interval (15m default) this whole mechanism exists to shortcut past.
const defaultInvalidateDebounce = 5 * time.Second

// MembershipEvent is the invalidation hint one upstream event carries: which
// scope may have changed. It names no tuple, no credential, and no decision
// to write anything — seeing one is never sufficient reason to mutate
// SpiceDB, only reason to ask the reconcile to look again.
type MembershipEvent struct {
	// Scope is the upstream scope id the event names (a Slack channel id,
	// for the "slack" Kind). Required — Handle refuses an event that
	// doesn't name one.
	Scope relsync.ScopeID
}

// invalidatorOption configures a NewInvalidator call.
type invalidatorOption func(*Invalidator)

// withDebounce overrides defaultInvalidateDebounce. Package-private: nothing
// outside this package constructs an Invalidator yet (no subscriber wiring
// exists), so there is no consumer to export it for; a task that adds one
// can export it then, the same way NewReconciler's own optional fields are
// exported only because operator main.go and the e2e harness already need
// them.
func withDebounce(d time.Duration) invalidatorOption {
	return func(inv *Invalidator) { inv.debounce = d }
}

// Invalidator turns upstream membership events into a debounced wake of one
// RelationshipSource's reconcile. Handle's entire job is bookkeeping —
// marking a scope dirty and (re)starting a timer — never touching SpiceDB;
// OnWake is what a caller wires to an actual reconcile trigger (a
// controller-runtime enqueue), and that reconcile is what runs relsync.Pass
// and writes.
//
// One Invalidator serves exactly one RelationshipSource. Reconcile's own
// findIncumbent already guarantees at most one RelationshipSource legitimately
// claims a given spec.kind — two CRs naming the same kind park all but the
// oldest — so "the invalidator for kind=slack" and "the RelationshipSource
// that owns slack_channel#member" are necessarily the same CR. There is no
// (source, scope) ambiguity to resolve inside Handle, only which scope,
// within this one source, is dirty.
//
// Every Slack app installed in a channel gets its OWN copy of a membership
// event, so N per-agent apps in one channel produce N calls to Handle for
// what is, on the wire, one event. Marking a scope dirty is idempotent (a
// set, not a counter) and the debounce timer restarts on every Handle call —
// so N duplicates arriving in quick succession collapse into exactly one
// OnWake, never N. No dedup layer sits in front of Handle; this property is
// what stands in for one.
type Invalidator struct {
	// Source names the RelationshipSource this invalidator wakes. Required:
	// Handle refuses an event on an Invalidator with no Source configured
	// rather than silently accumulating dirty scopes under a key nothing
	// will ever be asked to reconcile — see AGENTS.md's no-silent-errors
	// rule.
	Source types.NamespacedName

	// OnWake is called with Source, at most once per debounce window, once
	// Handle has seen no further event for debounce's duration. Wiring this
	// to actually enqueue a controller-runtime reconcile.Request is a later
	// task's job — nil is a valid, inert Invalidator (e.g. a test that only
	// inspects DirtyScopes never needs to set it).
	OnWake func(types.NamespacedName)

	debounce time.Duration

	mu    sync.Mutex
	dirty map[relsync.ScopeID]struct{}
	timer *time.Timer
}

// NewInvalidator builds an Invalidator serving source, with the default 5s
// debounce unless overridden.
func NewInvalidator(source types.NamespacedName, opts ...invalidatorOption) *Invalidator {
	inv := &Invalidator{
		Source:   source,
		debounce: defaultInvalidateDebounce,
	}
	for _, opt := range opts {
		opt(inv)
	}
	return inv
}

// Handle marks ev's scope dirty for inv.Source and (re)starts the debounce
// timer. It never calls WriteRelationships or DeleteRelationships on any
// path, directly or through OnWake — that only happens once OnWake's caller
// re-fetches and diffs, strictly after Handle has already returned.
//
// Two shapes of malformed event are recorded rather than dropped, per
// AGENTS.md's no-silent-errors rule: an event naming no scope, and an event
// arriving at an Invalidator with no Source configured — the latter is a
// wiring bug (nothing names which RelationshipSource should ever be woken
// for it), and marking a scope dirty under an empty NamespacedName would
// accumulate state no reconcile is ever asked to look at, silently.
func (inv *Invalidator) Handle(ctx context.Context, ev MembershipEvent) error {
	logger := log.FromContext(ctx)

	if inv.Source == (types.NamespacedName{}) {
		err := fmt.Errorf("relationshipsource: invalidate: no Source configured on this Invalidator; event for scope %q dropped", ev.Scope)
		logger.Info("RelationshipSource invalidate: misconfigured invalidator", "scope", ev.Scope, "err", err.Error())
		return err
	}
	if ev.Scope == "" {
		err := fmt.Errorf("relationshipsource: invalidate: event for %s names no scope", inv.Source)
		logger.Info("RelationshipSource invalidate: event missing scope", "source", inv.Source, "err", err.Error())
		return err
	}

	inv.mu.Lock()
	if inv.dirty == nil {
		inv.dirty = map[relsync.ScopeID]struct{}{}
	}
	inv.dirty[ev.Scope] = struct{}{}

	// Restart, not merely start: a new event within an already-running
	// debounce window must push the wake back out, or a steady trickle of
	// distinct events would still wake once per event instead of once per
	// quiet period. Stop's return value is ignored deliberately — false
	// (already fired or never started) is exactly as fine here as true.
	if inv.timer != nil {
		inv.timer.Stop()
	}
	inv.timer = time.AfterFunc(inv.debounce, inv.wake)
	inv.mu.Unlock()

	return nil
}

// wake is the debounce timer's callback: fires at most once per quiet
// window, and — because it only ever runs as time.AfterFunc's own goroutine —
// never synchronously from inside Handle. OnWake is read and invoked outside
// the lock so a caller's OnWake is free to call back into Handle or
// DirtyScopes without deadlocking against this Invalidator's own mutex.
func (inv *Invalidator) wake() {
	inv.mu.Lock()
	inv.timer = nil
	onWake := inv.Source
	fn := inv.OnWake
	inv.mu.Unlock()

	if fn != nil {
		fn(onWake)
	}
}

// DirtyScopes returns every scope currently marked dirty for inv.Source,
// sorted for a deterministic comparison. Non-destructive: clearing a scope
// once its reconcile has actually re-fetched it is that reconcile's job (via
// PassInput.OnlyScopes), never Handle's or the debounce timer's — an event
// that arrives while a pass is already in flight for the same scope must not
// be lost just because DirtyScopes happened to be read a moment earlier.
func (inv *Invalidator) DirtyScopes() []relsync.ScopeID {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	out := make([]relsync.ScopeID, 0, len(inv.dirty))
	for s := range inv.dirty {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
