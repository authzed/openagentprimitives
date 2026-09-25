package toolguard

import (
	"context"
	"sync"
)

// The half-open probe slot is a *claim*, not a flag: Admit grants exactly one
// in-flight call the right to test a recovering breaker, and Record hands the
// claim back with an outcome. Half-open has no cool-off timer (only stateOpen
// consults one), so a claim nothing hands back denies every later call on that
// key with deniedBy "probe" for the life of the session — and when the key is
// an origin key, that is every tool of that MCP server.
//
// The runner's contained-call sequence has three reachable exits that return
// before PostToolCall — and therefore before Record — ever runs: a Pre Deny (a
// downstream gate, including a human-denied or timed-out approval), a Pre Halt,
// and a user-interrupt cancellation. Its fail-closed PreToolCall-executor-error
// branch is a fourth, currently defensive: pipeline.Executor.Run resolves every
// path to a verdict today and never returns a non-nil error.
//
// The ledger closes that hole structurally instead of asking each of those
// exits to remember a release call. The caller opens exactly one ledger per
// contained call and defers its release; Admit files every claim it grants
// into the ledger it finds in ctx, so claiming without filing is not
// expressible. However the call exits — including a panic — an unresolved
// claim goes back.
//
// A release that runs after Record already resolved the claim is a no-op:
// claims are single-use and matched by id, so a later call that legitimately
// re-took the slot in between is never disturbed.

// probeLedgerKey is the private ctx key for the per-call ledger.
type probeLedgerKey struct{}

// probeClaim identifies one granted half-open probe slot.
type probeClaim struct {
	reg *Registry
	key string
	id  uint64
}

// probeLedger collects the probe claims granted during one contained call.
// Safe for concurrent use: a single contained call is sequential, but the
// ledger is reachable from any goroutine holding the ctx.
type probeLedger struct {
	mu     sync.Mutex
	claims []probeClaim
}

func (l *probeLedger) file(c probeClaim) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.claims = append(l.claims, c)
}

// release hands back every claim no Record resolved. Idempotent: the ledger
// is drained, so a second call has nothing to hand back.
func (l *probeLedger) release() {
	l.mu.Lock()
	claims := l.claims
	l.claims = nil
	l.mu.Unlock()
	for _, c := range claims {
		c.reg.releaseProbe(c.key, c.id)
	}
}

// WithProbeLedger returns a child ctx carrying a fresh probe ledger plus the
// release func the caller MUST defer. Open exactly one per contained tool call
// (PreToolCall → Execute → PostToolCall), around the whole sequence:
//
//	ctx, releaseProbes := toolguard.WithProbeLedger(ctx)
//	defer releaseProbes()
//
// Any half-open breaker probe the PreToolCall guard claims under this ctx and
// no PostToolCall record resolves is handed back when release runs, so a
// denied, halted, or interrupted probe call cannot wedge the breaker for the
// rest of the session.
func WithProbeLedger(ctx context.Context) (context.Context, func()) {
	l := &probeLedger{}
	return context.WithValue(ctx, probeLedgerKey{}, l), l.release
}

// probeLedgerFrom returns the ledger carried by ctx, or nil when the caller
// opened none (see Guard.Eval, which logs that as a wiring regression).
func probeLedgerFrom(ctx context.Context) *probeLedger {
	l, _ := ctx.Value(probeLedgerKey{}).(*probeLedger)
	return l
}
