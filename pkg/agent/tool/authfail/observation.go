// Package authfail is the runner's credential-update CORROBORATION recorder:
// it turns "this tool call failed the way this provider's credentials fail"
// into the one durable observation the CredentialUpdateRequest reconciler
// consults when it cannot re-probe the provider to confirm the agent's claim.
//
// It adds no policy of its own to the two pieces it sits between:
// credupdate.IsAuthShaped owns the security-relevant match against a provider's
// declared authFailure: shape, and AgentSession.status.credentialAuthFailures is
// where the observation lands — the runner is the only component that sees a
// tool call's upstream outcome, so it is the only writer.
//
// What this package owns is the write policy, both halves load-bearing:
//
//   - The counter lives in memory; status is written only on the TRANSITION
//     into the observed state. A write per failing call would rewrite the object
//     on every call — an idempotency violation, not just churn.
//
//   - A SUCCESS on an origin REMOVES its observation. Without that, one stale
//     failure corroborates a healthy credential for the rest of the session and
//     helps talk a human into re-entering a working token. "Success" means the
//     origin AUTHENTICATED the call, not that it returned a non-error result: an
//     MCP server answering HTTP 200 with a tool-level isError, or a CLI exiting 0
//     without producing a declared secret file, both prove the credential works
//     and both arrive as error results — and the agent picks the arguments, so it
//     can manufacture that shape on demand. The evidence rides on
//     credupdate.Observation.OriginAuthenticated; ObserveFailure routes it to Clear.
//
// # Convergence
//
// Both halves concern a SINGLE origin's state and tool calls run concurrently,
// so Record and Clear only publish the DESIRED state; a per-origin write mutex
// serializes reconciliation, and the desired state is re-read AFTER that mutex
// is taken so the last logical operation wins. The obvious alternative — claim
// the transition, release the state lock, then write — lets two goroutines land
// patches in the opposite order to their decisions: a Clear lands, an in-flight
// Record lands on top, and memory believes there is no entry while status
// carries one. Every later success then short-circuits and the observation is
// stranded for the session.
//
// A write that ERRORS leaves the origin UNKNOWN rather than assuming it did not
// land — a patch that committed but whose response was lost would strand an
// entry the same way. Unknown forces the next reconcile to write.
//
// # Write volume
//
// Writes are O(transitions): a steady run of failures is one write, a steady run
// of successes zero, a FLAPPING origin two per alternation — each a Get+Patch on
// the tool hot path. Bounded and semantically correct, but worth knowing before
// adding hysteresis, which would trade away how fast a success retracts a stale
// observation.
package authfail

import (
	"context"
	"sync"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credupdate"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

// StatusWriter is the AgentSession status half of the recorder, an interface
// because the NUMBER of writes is the behavior here and a fake is the only way
// to count them without a cluster. Satisfied by *runner.StatusPatcher: an
// optimistic-locked read-modify-write with conflict retry, on a client whose
// per-session Role grants only `get agentsessions` + `patch
// agentsessions/status`, both pinned to this session's name.
type StatusWriter interface {
	// RecordCredentialAuthFailure upserts origin's observation with count.
	RecordCredentialAuthFailure(ctx context.Context, origin string, count int32) error
	// ClearCredentialAuthFailure removes origin's observation.
	ClearCredentialAuthFailure(ctx context.Context, origin string) error
}

// Recorder observes per-origin auth failures for one AgentSession.
//
// The zero value is not usable; build one with New. A nil *Recorder is a safe
// no-op on every method, so a runner assembled without one (no status writer,
// no session CR) simply records nothing rather than forcing a nil check at
// each call site.
type Recorder struct {
	w StatusWriter

	// authFailureFor maps a tool origin to the provider's declared authFailure:
	// block, nil when the origin resolves to no provider or declares no block.
	// Resolved ONCE at runner startup: origin → provider is several API reads,
	// too costly per failed call for a signal checked at human latency.
	//
	// A nil func or nil result means "no declared shape to match", which
	// IsAuthShaped reads as NOT corroborated — never a default-yes.
	authFailureFor func(origin string) *provider.AuthFailure

	// mu guards every map below. It is never held across a status write.
	mu sync.Mutex
	// consecutive counts auth-shaped failures per origin SINCE the last clear.
	// In memory on purpose — see the package doc.
	consecutive map[string]int32
	// desired is what status SHOULD say for an origin: true = carry an entry.
	// Published by Record/Clear, read by reconcile.
	desired map[string]bool
	// written is what the last completed write says status DOES say. It is the
	// transition detector — reconcile writes only when it disagrees with
	// desired — and it is tri-state so a failed write can record "unknown"
	// rather than a guess (see writeState).
	written map[string]writeState
	// writeLocks serializes reconciliation per origin, so two concurrent
	// observers of one origin can never land their patches out of order. One
	// entry per origin the session ever touches; bounded by the tool set.
	writeLocks map[string]*sync.Mutex
}

// writeState is what the recorder believes status currently holds for an
// origin.
type writeState uint8

const (
	// writeAbsent means status carries no entry for this origin. Deliberately the
	// ZERO value: a never-seen origin genuinely has no entry, and every
	// successful call reaches Clear — if "never seen" forced a write, a session
	// whose credentials all work would patch status on every single call.
	writeAbsent writeState = iota
	// writePresent means status carries an entry for this origin.
	writePresent
	// writeUnknown means the last write FAILED and may or may not have committed,
	// so it always forces the next reconcile to write. Assuming a failed write
	// did not land is how an entry gets stranded when the patch committed and
	// only the response was lost. Reachable ONLY from a write error.
	writeUnknown
)

// New builds a Recorder over w.
//
// authFailureFor resolves a tool origin to the provider's authFailure: block;
// nil disables corroboration entirely (nothing is ever recorded).
//
// existing seeds the transition detector from the observations already on the
// session's status. It matters on a RESTART: a fresh process has an empty map
// while status can still carry an entry the previous process wrote, and without
// the seed the first successful call would see "nothing recorded", skip the
// clear, and strand a stale observation for the rest of the session.
func New(w StatusWriter, authFailureFor func(origin string) *provider.AuthFailure, existing []spiceboxv1alpha1.CredentialAuthFailure) *Recorder {
	r := &Recorder{
		w:              w,
		authFailureFor: authFailureFor,
		consecutive:    map[string]int32{},
		desired:        map[string]bool{},
		written:        map[string]writeState{},
		writeLocks:     map[string]*sync.Mutex{},
	}
	for i := range existing {
		if existing[i].Origin == "" {
			continue
		}
		// Both sides are seeded: status HAS the entry (written), and until a
		// success says otherwise it SHOULD keep it (desired). Seeding only
		// `written` would make the very next failure look like a transition
		// and re-write an entry that is already there.
		r.written[existing[i].Origin] = writePresent
		r.desired[existing[i].Origin] = true
	}
	return r
}

// ObserveFailure classifies obs against origin's provider and records it when it
// matches. Callers hand over only what they SAW; the classification belongs to
// credupdate.IsAuthShaped and is not duplicated here.
//
// Call it for ANY call that returned an error result. One such shape is evidence
// of the opposite: an obs with OriginAuthenticated set describes a call the auth
// layer accepted and ran, whose error came from the tool rather than the
// credential, and it CLEARS just as a clean success does. That is why this
// method is the choke point rather than each caller's `if IsError` — a caller
// that forgot the distinction would silently strand observations.
//
// A plainly successful call may call Clear directly.
func (r *Recorder) ObserveFailure(ctx context.Context, origin string, obs credupdate.Observation) error {
	if r == nil || origin == "" {
		return nil
	}
	if obs.OriginAuthenticated {
		return r.Clear(ctx, origin)
	}
	var af *provider.AuthFailure
	if r.authFailureFor != nil {
		af = r.authFailureFor(origin)
	}
	return r.Record(ctx, origin, credupdate.IsAuthShaped(obs, af))
}

// Record notes one already-classified failure at origin.
//
// authShaped=false records nothing and, deliberately, clears nothing. This is
// the NO-EVIDENCE path — connection refused, DNS, timeout, 5xx — where the
// provider's auth layer never formed an opinion, so nothing may retract an
// observation the way a genuine success does. Do NOT "fix" the asymmetry by
// making it clear: that case and the authenticated-then-tool-error case
// (ObserveFailure's) both arrive as error results and mean opposite things.
//
// authShaped=true increments the in-memory counter, marks the origin as wanting
// an entry, and reconciles — writing only if status does not already agree.
func (r *Recorder) Record(ctx context.Context, origin string, authShaped bool) error {
	if r == nil || origin == "" || !authShaped {
		return nil
	}
	r.mu.Lock()
	r.consecutive[origin]++
	r.desired[origin] = true
	r.mu.Unlock()
	return r.reconcile(ctx, origin)
}

// Clear removes origin's observation after a SUCCESSFUL call there: a
// credential that just worked is not one that needs replacing. It writes only
// when status does not already agree — every successful tool call reaches this
// method, so an unconditional write would put a read+patch on every call.
func (r *Recorder) Clear(ctx context.Context, origin string) error {
	if r == nil || origin == "" {
		return nil
	}
	r.mu.Lock()
	delete(r.consecutive, origin)
	r.desired[origin] = false
	r.mu.Unlock()
	return r.reconcile(ctx, origin)
}

// reconcile drives one origin's status toward its desired state.
//
// The per-origin write lock makes the compare and the write ATOMIC against other
// observers of the same origin, so two concurrent calls can never land patches
// in the opposite order to their decisions. Desired state is re-read AFTER the
// lock, so whoever reconciles last writes the last logical intent and a
// superseded caller correctly writes nothing. Being per origin, a slow write
// against one MCP server never blocks an observation about another.
func (r *Recorder) reconcile(ctx context.Context, origin string) error {
	wl := r.writeLock(origin)
	wl.Lock()
	defer wl.Unlock()

	r.mu.Lock()
	want := r.desired[origin]
	have := r.written[origin]
	count := r.consecutive[origin]
	r.mu.Unlock()

	// Status already says what we want, and we KNOW it does (writeUnknown never
	// short-circuits — see its doc).
	if (want && have == writePresent) || (!want && have == writeAbsent) {
		return nil
	}

	var err error
	if want {
		err = r.w.RecordCredentialAuthFailure(ctx, origin, count)
	} else {
		err = r.w.ClearCredentialAuthFailure(ctx, origin)
	}

	r.mu.Lock()
	switch {
	case err != nil:
		// The patch may or may not have committed. Recording either concrete
		// state would be a guess, and guessing "did not commit" is how an entry
		// gets stranded: a later success would believe status is already clean
		// and never issue the clear.
		r.written[origin] = writeUnknown
	case want:
		r.written[origin] = writePresent
	default:
		r.written[origin] = writeAbsent
	}
	r.mu.Unlock()
	return err
}

// writeLock returns origin's write mutex, creating it on first use.
func (r *Recorder) writeLock(origin string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	wl, ok := r.writeLocks[origin]
	if !ok {
		wl = &sync.Mutex{}
		r.writeLocks[origin] = wl
	}
	return wl
}
