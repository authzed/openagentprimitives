package provenance

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// ErrEntryAppendedDispatchPut is returned when an append-only Put reaches a
// SigningMemory from inside its own SignalEntryAppended dispatch — a
// ScopeHooks reaction Put()ing back through the same wrapper that raised the
// signal. See the comment inside Put for why this cannot be performed safely
// on the calling goroutine.
var ErrEntryAppendedDispatchPut = errors.New("signingmem: refusing an append-only Put from inside an entry-appended dispatch")

// SigningMemory decorates a memory.Memory so that Put on an append-only Kind
// seeds the publisher's chain (once per scope) and signs the entry before it
// reaches the inner store. Query, Search, and SendSignal pass through unchanged
// via the embedded Memory.
//
// Every append-only writer in the process (authz-decision recording, approval
// requests, audit-log appends, lifecycle events, transcript writes — on a
// detached goroutine or the live turn) goes through one SigningMemory per
// session, so serializing Sign+Put here covers all of them from a single choke
// point; see Put.
type SigningMemory struct {
	memory.Memory
	signer *Signer

	mu      sync.Mutex                   // guards scopeMu
	scopeMu map[memory.Scope]*sync.Mutex // per-scope append serialization
}

// NewSigningMemory wraps inner so append-only Puts are attested by
// signer.
func NewSigningMemory(inner memory.Memory, signer *Signer) *SigningMemory {
	return &SigningMemory{Memory: inner, signer: signer, scopeMu: map[memory.Scope]*sync.Mutex{}}
}

// lockForScope returns the mutex serializing append-only Sign+Put for
// scope, creating it on first use. Keyed by the full memory.Scope (not
// just ID) so lock granularity exactly matches signer.chains' key
// granularity — one lock per chain, no coarser and no finer.
func (m *SigningMemory) lockForScope(scope memory.Scope) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lk, ok := m.scopeMu[scope]
	if !ok {
		lk = &sync.Mutex{}
		m.scopeMu[scope] = lk
	}
	return lk
}

// errEmptyIDSign refuses to sign an append-only entry with no ID, at the
// signing choke point rather than three layers away.
//
// EntryDigest signs ID along with everything else (digest.go:30-53), and
// httpsrv.putEntry assigns a real ID whenever the body has none
// (`if e.ID == "" { e.ID = memory.NewID(k) }`) BEFORE the append-only
// facade's verify-on-write recomputes the digest over the entry AS STORED.
// Signing an empty ID therefore produces a signature that can never
// re-verify the moment the server does its job — every such call was
// rejected with ErrBadProvenance in production while every test that built
// its own ID-bearing fixture stayed green. Refusing here turns a signature
// that would fail three layers down, unreadably, into an immediate, named
// caller error.
func errEmptyIDSign(kind string) error {
	return fmt.Errorf(
		"signingmem: refusing to sign a %q entry with no ID: a signature over an ID the server is about to assign can never re-verify — set ID before Put/PutToPool, e.g. via memory.NewID(k)",
		kind)
}

func errZeroTimeSign(kind string) error {
	return fmt.Errorf("signingmem: refusing to sign a %q entry with zero CreatedAt: set its timestamp before Put/PutToPool so storage preserves the signed digest", kind)
}

// Put signs append-only entries before forwarding them; mutable-Kind
// entries pass through untouched (Provenance stays nil).
func (m *SigningMemory) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	if memory.KindAppendOnly(e.Kind) {
		if e.ID == "" {
			return memory.Entry{}, errEmptyIDSign(e.Kind)
		}
		if e.CreatedAt.IsZero() {
			return memory.Entry{}, errZeroTimeSign(e.Kind)
		}
		// A ScopeHooks reaction to Local's entry-appended fan-out (memory.
		// InEntryAppendedDispatch) must not reach lockForScope below: the Put that
		// raised the signal is still on this exact goroutine's call stack, holding
		// lk for e.Scope until its own m.Memory.Put returns — and dispatch to
		// ScopeHooks happens synchronously, inside that call, before it returns.
		// lk is a plain sync.Mutex, not reentrant, so a second Lock from a hook's
		// reactive Put for the SAME scope would be this goroutine deadlocking on
		// itself, not two goroutines contending. There is no safe way to take the
		// lock on this goroutine, so the write is refused rather than attempted:
		// reporting success here would tell the caller its entry was signed and
		// persisted when it was silently discarded — see InEntryAppendedDispatch.
		if memory.InEntryAppendedDispatch(ctx) {
			return memory.Entry{}, fmt.Errorf(
				"%w: kind %q, scope %s: the per-scope signing lock is already held by the Put that raised the signal, on this same goroutine",
				ErrEntryAppendedDispatchPut, e.Kind, e.Scope.ID)
		}
		// Sign (which advances the in-memory chain head) and the durable Put
		// must be atomic per scope. Without this lock two concurrent same-scope
		// appends interleave: G1 signs seq N, G2 signs seq N+1 before G1's Put
		// lands; if G1's Put then fails, InvalidateSeed discards the seed for a
		// re-derive — but G2 already built seq N+1 on top of the phantom seq N,
		// which never lands durably. The chain gaps (N missing) and forks (N+1's
		// PrevHash points at an entry no one stored).
		//
		// Holding the lock across the durable m.Memory.Put (a possibly
		// network/Postgres write) is intentional, not an oversight: chain
		// integrity requires Sign and Put to commit as one unit, and append-only
		// writes are audit/transcript records, not a latency-critical hot path.
		// lockForScope keys per scope, so unrelated sessions never contend.
		lk := m.lockForScope(e.Scope)
		lk.Lock()
		defer lk.Unlock()

		if err := m.signer.EnsureSeeded(ctx, m.Memory, e.Scope); err != nil {
			return memory.Entry{}, fmt.Errorf("seed chain for %s: %w", e.Scope.ID, err)
		}
		if err := m.signer.Sign(&e); err != nil {
			return memory.Entry{}, fmt.Errorf("sign %s/%s: %w", e.Kind, e.ID, err)
		}
		out, err := m.Memory.Put(ctx, e)
		if err != nil {
			// Sign already advanced the in-memory chain head, but this entry
			// did NOT land. Invalidate the seed so the next append re-derives
			// the head from durable storage instead of building on a phantom
			// entry — otherwise the chain gaps + forks (see
			// signingmem_test.go and the audit-verify incident).
			m.signer.InvalidateSeed(e.Scope)
			return memory.Entry{}, err
		}
		// A nil error is NOT proof the signed entry landed at the position just
		// minted. memory.Local defines a byte-identical re-put of an append-only
		// entry as idempotent: it returns the ALREADY-STORED entry — provenance
		// untouched, at its original seq — with no error, because its
		// equivalence check deliberately excludes Provenance (a retried write
		// re-signs an identical payload). The minted seq was then never
		// persisted, stranding the head one position ahead of the durable tail
		// exactly as a rejected Put does. Ask the store which chain position it
		// actually holds rather than assuming it is ours.
		if !samePosition(e.Provenance, out.Provenance) {
			m.signer.InvalidateSeed(e.Scope)
		}
		return out, nil
	}
	return m.Memory.Put(ctx, e)
}

// PutToPool signs an append-only entry before forwarding it into a resource
// pool, the same way Put signs one before forwarding it to the caller's own
// scope — see Put's own comments for why Sign and the durable write must be
// atomic per scope, why a rejected write must not strand the in-memory head,
// and why a nil error is not proof the signed position landed. This method
// mirrors that discipline against poolScope rather than e.Scope, because the
// entry lands at the pool, not at sessionScope.
//
// FAILS LOUDLY — a distinct, named error identifying the concrete inner
// type — when m.Memory does not implement memory.PoolWriter, rather than
// silently declining the write. SigningMemory embeds Memory as an INTERFACE
// field, so Go's method promotion never forwards a concrete inner's extra
// methods on its own; an operator reading this error learns exactly which
// concrete type behind the signing wrapper needs to grow the capability,
// instead of chasing a permission-shaped refusal that was never about
// permissions.
//
// e.Scope is stamped to poolScope before signing (overwriting whatever the
// caller set, or left zero) because EntryDigest signs e.Scope, and the
// server forces the STORED entry's scope to be exactly the pool the request
// line named — signing anything else would produce a signature that could
// never verify against what actually lands.
func (m *SigningMemory) PutToPool(ctx context.Context, sessionScope, poolScope memory.Scope, e memory.Entry) (memory.Entry, error) {
	pw, ok := m.Memory.(memory.PoolWriter)
	if !ok {
		return memory.Entry{}, fmt.Errorf(
			"signingmem: PutToPool: the memory behind this signing wrapper (%T) does not implement memory.PoolWriter — pool writes are not wired for this backend",
			m.Memory)
	}

	e.Scope = poolScope

	if !memory.KindAppendOnly(e.Kind) {
		return pw.PutToPool(ctx, sessionScope, poolScope, e)
	}

	if e.ID == "" {
		return memory.Entry{}, errEmptyIDSign(e.Kind)
	}
	if e.CreatedAt.IsZero() {
		return memory.Entry{}, errZeroTimeSign(e.Kind)
	}

	// See Put's identical guard: a ScopeHooks reaction to Local's
	// entry-appended fan-out must not re-enter lockForScope on this same
	// goroutine.
	if memory.InEntryAppendedDispatch(ctx) {
		return memory.Entry{}, fmt.Errorf(
			"%w: kind %q, scope %s: the per-scope signing lock is already held by the Put that raised the signal, on this same goroutine",
			ErrEntryAppendedDispatchPut, e.Kind, poolScope.ID)
	}

	lk := m.lockForScope(poolScope)
	lk.Lock()
	defer lk.Unlock()

	// The seed reads the POOL back, and reading a pool is its own addressing
	// problem — which is why the inner is wrapped rather than passed directly.
	//
	// EnsureSeeded resumes this chain by querying poolScope, exactly as Put's
	// seeding does for a session scope. Over an httpclient-shaped inner a plain
	// Query cannot express that: Client.Query builds its URL from scopePath,
	// which assumes a "<ns>/<name>" session ID, so a "resource" scope produced
	// "/memory/observation/dossier:d-1/" and the real server answered 404. That
	// is not ErrKindNotSessionReadable, so SeedFromMemory returned it and the
	// FIRST pool write failed outright, in every mode — not, as an earlier
	// comment here claimed, a chain discontinuity that degraded loudly after a
	// restart. poolQuerier routes the read through memory.PoolReader, the read
	// half of the addressing PutToPool already had.
	if err := m.signer.EnsureSeeded(ctx, poolQuerier{Memory: m.Memory, sessionScope: sessionScope}, poolScope); err != nil {
		return memory.Entry{}, fmt.Errorf("seed chain for %s: %w", poolScope.ID, err)
	}
	if err := m.signer.Sign(&e); err != nil {
		return memory.Entry{}, fmt.Errorf("sign %s/%s: %w", e.Kind, e.ID, err)
	}
	out, err := pw.PutToPool(ctx, sessionScope, poolScope, e)
	if err != nil {
		m.signer.InvalidateSeed(poolScope)
		return memory.Entry{}, err
	}
	if !samePosition(e.Provenance, out.Provenance) {
		m.signer.InvalidateSeed(poolScope)
	}
	return out, nil
}

var _ memory.PoolWriter = (*SigningMemory)(nil)

// poolQuerier is a memory.Memory view whose Query addresses a RESOURCE pool
// the way the transport underneath requires, so SeedFromMemory can keep taking
// a plain memory.Memory and asking one question.
//
// It routes through memory.PoolReader when the inner has it — over an
// *httpclient.Client that is the difference between a served listing and a 404
// — and otherwise queries the scope directly, which is what an in-process
// facade does correctly on its own: memory.Local addresses any scope by value,
// and its own door still decides whether this caller may read that pool.
//
// The fallback is not the silent kind. A store that can neither route a pool
// read nor address one by value ERRORS on the query, the error propagates out
// of EnsureSeeded, and the write fails with the scope named — which is exactly
// how this defect surfaced once someone ran it. Contrast PoolWriter, where the
// missing capability had to be refused at the assertion because a pool write
// CANNOT be expressed as a scope-addressed Put at all.
type poolQuerier struct {
	memory.Memory
	// sessionScope is what the request authenticates AS; the pool scope on the
	// query is what it addresses. Both are needed: over HTTP the URL names the
	// session the bearer token authorizes, and the pool rides the request line.
	sessionScope memory.Scope
}

func (p poolQuerier) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	if pr, ok := p.Memory.(memory.PoolReader); ok && memory.IsResourceScope(q.Scope) {
		return pr.QueryPool(ctx, p.sessionScope, q.Scope, q)
	}
	return p.Memory.Query(ctx, q)
}

// samePosition reports whether stored is the attestation signed carries: the
// same publisher and key at the same chain position. A nil or divergent
// stored provenance means this Put did not persist the position just minted,
// so the in-memory head has to be re-derived from durable storage.
//
// Deliberately conservative — anything short of an exact match re-seeds. A
// false re-seed costs one query on the next append; a missed one gaps and
// forks the chain, which `oap audit verify` cannot tell from tampering.
func samePosition(signed, stored *memory.Provenance) bool {
	return signed != nil && stored != nil &&
		stored.Publisher == signed.Publisher &&
		stored.KeyID == signed.KeyID &&
		stored.Seq == signed.Seq &&
		stored.PrevHash == signed.PrevHash
}
