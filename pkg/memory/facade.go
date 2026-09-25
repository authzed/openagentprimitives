package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/go-logr/logr"
)

// ProvenanceVerifier validates the provenance envelope on an append-only write.
// The operator injects one so the facade enforces signing without importing
// crypto. caller is CallerFrom(ctx) — the USER, and "" for both in-process
// operator writes and per-session bearers, so binding an entry's publisher to
// its author must read TokenSessionFrom(ctx), not caller.
type ProvenanceVerifier interface {
	VerifyEntry(ctx context.Context, caller string, e Entry) error
}

// Memory is the caller-facing memory interface. Local (operator) and
// httpclient.Client (runner/ap/channelsd) implement it.
type Memory interface {
	Put(ctx context.Context, e Entry) (Entry, error)
	Query(ctx context.Context, q Query) (QueryResult, error)
	Search(ctx context.Context, req SearchRequest) (MergedSearchResult, error)
	SendSignal(ctx context.Context, sig Signal) error
}

// Local is the in-process Memory implementation. It wraps a Backend,
// owns the Kind registry's per-(Scope, Kind) ScopeHooks
// materialization, and fans out Signal dispatch.
type Local struct {
	backend    Backend
	authz      Authorizer         // nil = no authorization checks
	provVerify ProvenanceVerifier // nil = no verify-on-write
	search     []SearchProvider
	searcher   Searcher
	logger     logr.Logger
	hooksMu    sync.Mutex
	// scope+kindName → ScopeHooks. Hooks are lazily materialized on
	// first SendSignal touching the scope; subsequent signals reuse.
	hooks map[hooksKey]ScopeHooks
}

// Local satisfies the Memory interface.
var _ Memory = (*Local)(nil)

type hooksKey struct {
	scopeKind string
	scopeID   string
	kindName  string
}

// entryAppendedDispatchKey marks a ctx as carrying an in-flight
// SignalEntryAppended dispatch. See withEntryAppendedDispatch.
type entryAppendedDispatchKey struct{}

// withEntryAppendedDispatch marks ctx as dispatching a SignalEntryAppended
// signal to ScopeHooks. The mark rides every ctx a hook derives from it —
// including a nested one, however many Put/SendSignal hops deep — because
// context.WithValue always preserves ancestor values.
//
// Put consults InEntryAppendedDispatch before emitting the signal at all: a
// ScopeHooks that appends in reaction to a signal — each write minting a
// freshly randomized entry ID, so the idempotent-re-put short-circuit never
// applies — would otherwise recurse without bound once its own append-only
// Put raised the same signal again. The mark is set once, at the facade's
// single dispatch choke point, so it protects every current and future
// ScopeHooks implementation that appends in response to a signal, without
// requiring each one to remember to guard itself.
func withEntryAppendedDispatch(ctx context.Context) context.Context {
	return context.WithValue(ctx, entryAppendedDispatchKey{}, true)
}

// InEntryAppendedDispatch reports whether ctx already carries an in-flight
// SignalEntryAppended dispatch. Exported (the mark itself stays unforgeable —
// only withEntryAppendedDispatch, unexported, can set it) because
// pkg/memory/provenance's SigningMemory needs to observe it too: SigningMemory
// holds a per-scope mutex across its own call into Put (Sign and the durable
// write must commit as one unit), and a ScopeHooks reaction reachable from
// inside that same call — via this dispatch — that Puts into the SAME scope
// through the SAME SigningMemory would re-enter that mutex on the same
// goroutine and deadlock, not merely recurse. Local.Put's own append-only
// write still happens once for a direct (unwrapped) caller — this flag only
// suppresses the SIGNAL a Put would otherwise emit, and lets a
// SigningMemory-wrapped caller suppress the WRITE itself, one layer up, where
// the deadlock would otherwise occur.
func InEntryAppendedDispatch(ctx context.Context) bool {
	v, _ := ctx.Value(entryAppendedDispatchKey{}).(bool)
	return v
}

// LocalOption is a functional option for NewLocal.
type LocalOption func(*Local)

// WithAuthorizer sets the Authorizer consulted on every Put/Query/Delete.
// Unset (the default) means no authorization checks.
func WithAuthorizer(a Authorizer) LocalOption {
	return func(l *Local) { l.authz = a }
}

// WithProvenanceVerifier enforces signed provenance on append-only writes:
// every such Put must carry an envelope verifying under a registered
// publisher key.
func WithProvenanceVerifier(v ProvenanceVerifier) LocalOption {
	return func(l *Local) { l.provVerify = v }
}

// WithSearchProviders registers search providers that receive index updates
// on Put/Delete/DeleteScope. These are kept separate from the Searcher so
// the facade can index into providers without importing the composite layer.
func WithSearchProviders(ps ...SearchProvider) LocalOption {
	return func(l *Local) { l.search = append(l.search, ps...) }
}

// WithSearcher sets the Searcher used by Local.Search. When nil (the
// default), Search returns ErrNoSearchProviders.
func WithSearcher(s Searcher) LocalOption {
	return func(l *Local) { l.searcher = s }
}

func WithLogger(l logr.Logger) LocalOption {
	return func(loc *Local) { loc.logger = l }
}

// NewLocal wraps b in an in-process facade. A nil Backend panics: every facade
// method dereferences it, so it is a programmer error, not a runtime condition.
func NewLocal(b Backend, opts ...LocalOption) *Local {
	if b == nil {
		panic("memory.NewLocal: nil Backend")
	}
	l := &Local{backend: b, hooks: map[hooksKey]ScopeHooks{}}
	for _, o := range opts {
		o(l)
	}
	return l
}

// ensureApproval is the capability door for the facade's data-access methods.
// A denial is logged as well as returned: it almost always means a caller
// reached the facade without minting an approval, and unlogged that surfaces
// only as a downstream 403/500 or a quietly broken feature. op is a short verb
// ("query"/"put"/…) for the log entry.
func (m *Local) ensureApproval(ctx context.Context, perm Permission, resource, op string) error {
	if err := EnsureApproval(ctx, perm, resource); err != nil {
		m.logger.Info("memory: capability door denied — caller lacks a required approval (likely a missing-mint bug)",
			"op", op, "perm", string(perm), "resource", resource)
		return err
	}
	return nil
}

// authorizeKindWrite is the PER-KIND half of the write door: may the credential
// this call arrived on author entries of this Kind at all?
//
// It gates the SESSION-CREDENTIAL door, identified by the token-session
// capability the memory HTTP handler attaches for exactly the per-session
// bearers (WithTokenSession). Three callers therefore pass untouched, all
// deliberate: the operator writing IN-PROCESS (controllers, the restart/fork
// copy, the lifecycle ScopeHooks fan-out — which strips the mark at its
// authorship hand-off); the system component tokens (channelsd, authzd), which
// authenticate as themselves; and the read-only webd token, refused a write
// route before it gets here.
//
// PRESENCE, not non-emptiness, is the test — a present-but-empty session is a
// wiring bug and is refused, per WithTokenSession's contract. An UNREGISTERED
// Kind is refused too: it has declared no authority, and "declared nothing"
// must not read as "anyone may write it".
func (m *Local) authorizeKindWrite(ctx context.Context, kind, op string) error {
	sess, tokenOriginated := TokenSessionFrom(ctx)
	if !tokenOriginated {
		return nil
	}
	k, registered := LookupKind(kind)
	if registered && k.WriteAuthority() == SessionWritten {
		return nil
	}
	authority := "unregistered"
	if registered {
		authority = k.WriteAuthority().String()
	}
	// Logged as well as returned: a refusal is either a real authorization event
	// or a legitimate writer nobody classified, and the caller — which sees only
	// a 403 — cannot tell them apart.
	m.logger.Info("memory: per-kind write door denied — a session credential may not author this Kind",
		"op", op, "kind", kind, "authority", authority,
		"session", sess.Namespace+"/"+sess.Name)
	return fmt.Errorf("%w: %q is %s", ErrKindNotSessionWritable, kind, authority)
}

// authorizeKindRead refuses a session credential that NAMES a Kind it may not
// read. In-process platform callers are unaffected.
//
// Loud, because an explicit ask for a platform-only Kind is either a bug or an
// attempt: answering it with an empty result would look like "there is nothing
// there", and the caller would carry on believing it had looked.
func (m *Local) authorizeKindRead(ctx context.Context, kinds []string, op string) error {
	who, gated := kindReadDoorSubject(ctx)
	if !gated {
		return nil
	}
	for _, kind := range kinds {
		if SessionMayRead(kind) {
			continue
		}
		m.logger.Info("memory: per-kind read door denied — this credential may not read this Kind",
			"op", op, "kind", kind, "caller", who)
		return fmt.Errorf("%w: %q is not session-readable", ErrKindNotSessionReadable, kind)
	}
	return nil
}

// dropUnreadableKinds removes platform-only entries from a session
// credential's results.
//
// The silent half, and deliberately so: a query that NARROWS on nothing
// legitimately sweeps every Kind in the scope, and erroring on it would make a
// broad query fail the moment any platform-only record existed in the session.
// The caller asked for "everything I may see", and this is what that means.
//
// Both halves are needed. Without the loud one, a targeted read of a hidden
// Kind would return empty and read as absence; without this one, a query
// naming no Kinds would return the hidden entries outright.
func (m *Local) dropUnreadableKinds(ctx context.Context, entries []Entry, op string) []Entry {
	who, gated := kindReadDoorSubject(ctx)
	if !gated || len(entries) == 0 {
		return entries
	}
	out := entries[:0:0]
	dropped := 0
	for _, e := range entries {
		if SessionMayRead(e.Kind) {
			out = append(out, e)
			continue
		}
		dropped++
	}
	if dropped > 0 {
		m.logger.Info("memory: per-kind read door filtered platform-only entries from a query",
			"op", op, "dropped", dropped, "caller", who)
	}
	return out
}

// Put validates the Entry against the registered Kind's prefix rules, then
// forwards to the backend.
//
// The door: a MUTABLE Kind requires an approval for (WriteMemory, e.Scope.ID)
// (bearer, SpiceDB, or system) or Put returns ErrMissingApproval before touching
// the backend. For an APPEND-ONLY Kind authorization IS provenance verification
// — a genuinely-new entry must pass the configured ProvenanceVerifier (with none
// configured the write is logged and allowed, for local/dev), after which the
// facade self-mints the internal AppendAudit approval proving the gate was
// satisfied. A WriteMemory or system approval can never substitute for either.
func (m *Local) Put(ctx context.Context, e Entry) (Entry, error) {
	// Every refusal in this block is about the ENTRY, not the store or the
	// caller's authorization, so each wraps ErrInvalidEntry: httpsrv maps that to
	// 400 and anything else to 5xx, and httpclient retries 5xx while treating 4xx
	// as permanent. A new validation refusal MUST wrap it (or a caller bug becomes
	// eight pointless retries); a fault must NOT (or a transient store failure
	// becomes a lost write).
	k, ok := LookupKind(e.Kind)
	if !ok {
		return Entry{}, fmt.Errorf("memory.Put: %w: unknown Kind %q", ErrInvalidEntry, e.Kind)
	}
	// Runs before the entry's own validation and before the append-only
	// pre-check's Get: a credential that may not author this Kind must learn
	// nothing about what is already stored under it, existence included.
	if err := m.authorizeKindWrite(ctx, e.Kind, "put"); err != nil {
		return Entry{}, err
	}
	if !strings.HasPrefix(e.ID, k.IDPrefix()) {
		return Entry{}, fmt.Errorf("memory.Put: %w: Entry.ID %q must start with Kind %q prefix %q",
			ErrInvalidEntry, e.ID, e.Kind, k.IDPrefix())
	}
	for i, lnk := range e.Links {
		target, found := LookupKind(lnk.Kind)
		if !found {
			continue // unknown-Kind links pass: only a registered target can be checked.
		}
		if !strings.HasPrefix(lnk.ID, target.IDPrefix()) {
			return Entry{}, fmt.Errorf(
				"memory.Put: %w: Entry.Links[%d].ID %q does not match Kind %q prefix %q",
				ErrInvalidEntry, i, lnk.ID, lnk.Kind, target.IDPrefix())
		}
	}
	// releaseAppendOnly ends the append-only critical section. It stays a no-op
	// for a mutable Kind, which never takes the lock: last-writer-wins is that
	// path's defined behavior, so serializing it would buy nothing and cost
	// contention on every label and session-scope write.
	releaseAppendOnly := func() {}
	if KindAppendOnly(e.Kind) {
		// Everything from the pre-check Get below to the backend.Put further down
		// is one check-then-write span, and it is not atomic on its own: two
		// writers of DIFFERENT content for this (scope, kind, id) would both find
		// nothing, both proceed, and the second would silently overwrite the
		// first.
		//
		// The racers are DISTINCT PROCESSES, not two goroutines of one. A single
		// writer process is already serialized per scope by its own
		// provenance.SigningMemory, which holds that lock across the whole HTTP
		// round trip. What nothing serializes is two components — runner,
		// channelsd, authzd — writing one (scope, kind, id) into the operator's
		// UNWRAPPED memLocal over the memory HTTP API, where each signs for
		// itself. That is plausible rather than theoretical because fact entry
		// IDs are deterministic while their content is not: factcontent.Record
		// derives the ID as idPrefix + EntryID(resourceType, resourceID, name),
		// so two observers of one subject collide on the ID by construction and
		// may disagree on the value.
		//
		// The conclusion is about WHERE the fix belongs: write-once is a property
		// of the store and has to be enforced at this facade, not by every caller
		// happening to hold a per-scope lock of its own.
		//
		// Deferred as the safety net for every return between here and the Put —
		// the idempotent re-put, the conflict, a verify failure, an authz denial —
		// and released explicitly the instant the Put lands. See the release call
		// for why it must not be held one statement longer than that, and
		// appendOnlyWriteLocks for what this lock does NOT protect.
		releaseAppendOnly = appendOnlyWriteLocks.Lock(appendOnlyKey{
			scopeKind: e.Scope.Kind, scopeID: e.Scope.ID, kind: e.Kind, id: e.ID,
		})
		defer releaseAppendOnly()

		existing, found, gerr := m.backend.Get(ctx, e.Scope, e.Kind, e.ID)
		if gerr != nil {
			return Entry{}, fmt.Errorf("append-only pre-check Get %s/%s: %w", e.Kind, e.ID, gerr)
		}
		if found {
			if entriesEquivalent(existing, e) {
				return existing, nil // idempotent re-put
			}
			return Entry{}, fmt.Errorf("%w: %s/%s %s", ErrAppendOnlyConflict, e.Scope.ID, e.Kind, e.ID)
		}
		// A genuinely-NEW append-only entry: an equivalent re-put already
		// returned, a divergent one already errored. Verify before
		// authz/backend.Put.
		if m.provVerify != nil {
			caller, _ := CallerFrom(ctx)
			if err := m.provVerify.VerifyEntry(ctx, caller, e); err != nil {
				return Entry{}, err
			}
		} else {
			m.logger.Info("append-only write without provenance verifier configured",
				"scope", e.Scope.ID, "kind", e.Kind, "id", e.ID)
		}
		// The ONLY way past this door is an AppendAudit approval, which the
		// unexported mintInternal produces here after provenance verification.
		// EnsureApproval refuses system for internal: perms, so no WriteMemory or
		// system approval can ever author a tamper-evident entry — the package
		// boundary enforces it against future refactors.
		ctx = WithApproval(ctx, mintInternal(AppendAudit, e.Scope.ID, "prov-verified"))
		if err := EnsureApproval(ctx, AppendAudit, e.Scope.ID); err != nil {
			return Entry{}, err
		}
	}
	if !KindAppendOnly(e.Kind) {
		if err := m.ensureApproval(ctx, WriteMemory, e.Scope.ID, "put"); err != nil {
			return Entry{}, err
		}
	}
	if m.authz != nil {
		if err := m.authz.AuthorizePut(ctx, e); err != nil {
			return Entry{}, err
		}
	}
	if err := m.backend.Put(ctx, e); err != nil {
		return Entry{}, err
	}
	// The check-then-write span is over: the entry is durable, so a competing
	// writer of this key may take the stripe now and will see it in its own
	// pre-check Get.
	//
	// Released HERE rather than left to the deferred call at function return,
	// because what follows runs arbitrary code synchronously on this goroutine:
	// the search-index fan-out, and the entry-appended dispatch into every
	// registered Kind's ScopeHooks. Two reasons, and the first is the one that
	// makes this mandatory rather than tidy.
	//
	// (a) SELF-DEADLOCK on a non-reentrant stripe. SendSignal dispatches hooks
	// synchronously, in a loop, on THIS goroutine. A hook that appends any
	// append-only entry straight through Local.Put re-enters this function and
	// tries to take a stripe for its own key — which is a different key, but
	// lands on the same stripe once in every 256. sync.Mutex is not reentrant,
	// so that is the goroutine deadlocking on itself: not two goroutines
	// contending, and not something a timeout unwinds. It is 1-in-256 rather
	// than certain, which is worse, because it would pass review and CI and
	// wedge the operator in production. Note that the analogous route through
	// provenance.SigningMemory is ALREADY refused up front by
	// ErrEntryAppendedDispatchPut, for the same reentrancy reason applied to its
	// per-scope lock; the direct route through Local.Put has no such guard, and
	// releasing here is what makes one unnecessary.
	//
	// (b) It would hold a stripe across a hook's network I/O — kgingestion
	// reaches Graphiti — stalling every unrelated entry whose key happens to
	// hash to that stripe, for the whole round trip.
	releaseAppendOnly()
	for _, sp := range m.search {
		if err := sp.Index(ctx, e.Scope, e.Kind, e.ID, e.Content); err != nil {
			m.logger.Info("search index failed", "provider", sp.Name(), "entry", e.ID, "err", err.Error())
		}
	}
	// Skipped when ctx already carries an in-flight entry-appended dispatch: this
	// Put is itself a ScopeHooks reaction to that signal, and re-emitting here
	// would recurse without bound if the reacting ScopeHooks appends — each
	// recursive write mints a fresh random ID, so the idempotent-re-put
	// short-circuit above never breaks the cycle. See withEntryAppendedDispatch.
	if KindAppendOnly(e.Kind) && !InEntryAppendedDispatch(ctx) {
		// The dispatch is the operator's own testimony that ITS durable write
		// landed, not a fresh capability grant to whoever called Put — so it runs
		// under a system approval rather than reusing the caller's ctx. By this
		// point ctx carries only the internal AppendAudit approval minted above
		// (an internal-tier approval that satisfies nothing but AppendAudit
		// itself), never a WriteMemory or system approval of the caller's own — so
		// without this wrap, SendSignal's own approval door would deny the
		// dispatch with ErrMissingApproval. The precedent is the hook side, not
		// the other in-process SendSignal caller: httpsrv's handleSignal reuses
		// r.Context() unwrapped because middleware already minted a WriteMemory
		// approval for that request, but lifecycle's and kgingestion's OnSignal
		// both wrap with WithSystemApproval for the same reason as here — a
		// signal arrives on whatever ctx the Put that raised it carried, which
		// holds no approval of the receiving code's own. A hook that errors must
		// not fail the write that already landed: logged rather than dropped, per
		// no-silent-errors.
		signalCtx := WithSystemApproval(ctx, "memory:entry_appended")
		signalCtx = withEntryAppendedDispatch(signalCtx)
		if serr := m.SendSignal(signalCtx, Signal{Kind: SignalEntryAppended, Scope: e.Scope}); serr != nil {
			m.logger.Info("entry-appended signal failed",
				"scope", e.Scope.ID, "kind", e.Kind, "entry", e.ID, "err", serr.Error())
		}
	}
	return e, nil
}

// entriesEquivalent reports whether two entries carry the same payload for
// append-only idempotency. Provenance is excluded — a retried write re-signs an
// identical payload. The comparison is over CANONICAL forms and implements
// "these two would sign to the same provenance digest, ignoring the envelope":
// content canonicalized as JSON, createdAt truncated to the microsecond a
// TIMESTAMPTZ column holds, tags as a sorted set, nil treated as empty for both
// tags and links (provenance.EntryDigest omitempty's them, so nil and empty
// already produce one signature).
//
// Raw bytes are the wrong instrument: one side has been through a backend. On
// postgres, content is JSONB (keys reordered, whitespace normalized) and
// created_at is TIMESTAMPTZ (microseconds), so a genuine retry of an identical
// write compared unequal and was answered ErrAppendOnlyConflict. The entry was
// never at risk — the first write had committed and the pre-check fails CLOSED
// — but the caller lost its success signal, and with it the signer's chain
// position, which then had to be re-derived from durable storage.
func entriesEquivalent(a, b Entry) bool {
	return bytes.Equal(CanonicalContent(a.Content), CanonicalContent(b.Content)) &&
		CanonicalTime(a.CreatedAt).Equal(CanonicalTime(b.CreatedAt)) &&
		slices.Equal(sortedCopy(a.Tags), sortedCopy(b.Tags)) &&
		slices.Equal(a.Links, b.Links)
}

// sortedCopy returns s sorted, without mutating s. Tags are a set: the digest
// that attests an entry sorts them, so two orderings of the same tags are one
// entry and must not read as a conflict.
func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// Get delegates to the backend. Returns (zero, false, nil) when the
// entry does not exist.
func (m *Local) Get(ctx context.Context, scope Scope, kind, id string) (Entry, bool, error) {
	return m.backend.Get(ctx, scope, kind, id)
}

// Delete is idempotent — deleting a non-existent entry is not an error.
//
// The door: a MUTABLE Kind requires an approval for (DeleteMemory, scope.ID)
// (bearer, SpiceDB, or system) or Delete returns ErrMissingApproval before
// touching the backend. An append-only Kind is refused outright — FIRST and
// unconditionally, regardless of approval. A per-session credential is
// additionally held to the Kind's WriteAuthority: removal is authorship, and a
// runner that could delete `cold_start_task` could replay the cold-start review
// that entry's existence is what closes.
func (m *Local) Delete(ctx context.Context, scope Scope, kind, id string) error {
	if KindAppendOnly(kind) {
		return fmt.Errorf("%w: %s/%s %s", ErrAppendOnlyKind, scope.ID, kind, id)
	}
	if err := m.authorizeKindWrite(ctx, kind, "delete"); err != nil {
		return err
	}
	if err := m.ensureApproval(ctx, DeleteMemory, scope.ID, "delete"); err != nil {
		return err
	}
	if m.authz != nil {
		if err := m.authz.AuthorizeDelete(ctx, scope, kind, id); err != nil {
			return err
		}
	}
	if err := m.backend.Delete(ctx, scope, kind, id); err != nil {
		return err
	}
	for _, sp := range m.search {
		if err := sp.DeleteIndex(ctx, scope, kind, id); err != nil {
			m.logger.Info("search delete-index failed", "provider", sp.Name(), "entry", id, "err", err.Error())
		}
	}
	return nil
}

// DeleteScope removes the MUTABLE (working) memory of a scope. It STRUCTURALLY
// CANNOT remove append-only kinds — the tamper-evident audit log (transcript,
// audit, authz-decision, tool-session, …) is PERMANENT and survives every scope
// deletion by design: a session "disappearing" must never take its logs with it,
// so no code path can erase them. Operator-only, in-process; not on the Memory
// interface, not over HTTP.
//
// The door: every call MUST carry an approval for (DeleteMemory, scope.ID).
// Only system (in-process operator/controller) callers reach this.
//
// authz.CleanupScope is deliberately NOT called: the scope's read grants must
// survive so the retained audit stays queryable after the session is gone.
func (m *Local) DeleteScope(ctx context.Context, scope Scope) error {
	if err := m.ensureApproval(ctx, DeleteMemory, scope.ID, "delete_scope"); err != nil {
		return err
	}
	res, err := queryProbingTruncation(ctx, m.backend, Query{Scope: scope, Limit: reindexMaxEntries})
	if err != nil {
		return fmt.Errorf("delete scope: query: %w", err)
	}
	for _, e := range res.Entries {
		if KindAppendOnly(e.Kind) {
			continue // append-only audit is PERMANENT — never removable
		}
		if err := m.backend.Delete(ctx, scope, e.Kind, e.ID); err != nil {
			return fmt.Errorf("delete scope: delete %s/%s: %w", e.Kind, e.ID, err)
		}
		for _, sp := range m.search {
			if err := sp.DeleteIndex(ctx, scope, e.Kind, e.ID); err != nil {
				m.logger.Info("delete-scope: search delete-index failed",
					"provider", sp.Name(), "scope", scope.ID, "kind", e.Kind, "id", e.ID, "err", err.Error())
			}
		}
	}
	if res.Truncated {
		m.logger.Info("delete-scope: hit entry limit; some ephemeral entries may remain",
			"scope", scope.ID, "limit", reindexMaxEntries)
	}
	return nil
}

// queryProbingTruncation runs q against b asking for one row past q.Limit, then
// trims the probe row away and reports its existence as QueryResult.Truncated.
//
// It is the ONLY way this package reads a limited query, so no read of a
// bounded window can report a partial answer as a complete one — the defect
// this exists for. The cost is one extra row per query, not a second scan.
func queryProbingTruncation(ctx context.Context, b Backend, q Query) (QueryResult, error) {
	probe := q
	probe.Limit = ProbeLimit(q.Limit)
	res, err := b.Query(ctx, probe)
	if err != nil {
		return res, err
	}
	res.Entries, res.Truncated = TrimProbe(res.Entries, q.Limit)
	return res, nil
}

// Query forwards to the backend. Callers wanting to observe degradation should
// check QueryResult.Partial / DroppedPredicates; correctness-wise every caller
// has a fallback path for empty results regardless of cause. A caller treating
// the answer as the complete match set must check QueryResult.Truncated, which
// says whether q.Limit ended the read before the matches did.
//
// The door: every call MUST carry an approval for (ReadMemory, q.Scope.ID)
// (bearer, SpiceDB, or system) or Query returns ErrMissingApproval before
// touching the backend — checked unconditionally, independent of whether an
// Authorizer is configured or a caller is present.
func (m *Local) Query(ctx context.Context, q Query) (QueryResult, error) {
	if err := m.ensureApproval(ctx, ReadMemory, q.Scope.ID, "query"); err != nil {
		return QueryResult{}, err
	}
	// A FieldEquals path naming no stored key can never match, so the backend
	// would answer empty and the caller could not tell that from "nothing
	// matched". Refuse rather than silently answer the wrong question; see
	// validateFieldPaths for why this is an error, not a DroppedPredicates entry.
	if err := validateFieldPaths(q); err != nil {
		m.logger.Info("memory: query rejected — unresolvable FieldEquals path",
			"scope", q.Scope.ID, "kinds", q.Kinds, "err", err.Error())
		return QueryResult{}, err
	}
	if err := m.authorizeKindRead(ctx, q.Kinds, "query"); err != nil {
		return QueryResult{}, err
	}
	res, err := queryProbingTruncation(ctx, m.backend, q)
	if err != nil {
		return res, err
	}
	res.Entries = m.dropUnreadableKinds(ctx, res.Entries, "query")
	if m.authz != nil {
		if _, hasCaller := CallerFrom(ctx); hasCaller {
			filtered, fErr := m.authz.AuthorizeQuery(ctx, res.Entries)
			if fErr != nil {
				return QueryResult{}, fErr
			}
			res.Entries = filtered
		}
	}
	return res, nil
}

// QueryAllScopes runs a cross-scope query against the backend. ADMIN-ONLY:
// there is deliberately no Authorizer post-filter here (the per-entry
// authorizer models per-session interact, which platform admins bypass by
// design) — callers MUST gate on the SpiceDB platform permissions first.
func (m *Local) QueryAllScopes(ctx context.Context, q CrossScopeQuery) (QueryResult, error) {
	if q.ScopeKind == "" {
		return QueryResult{}, fmt.Errorf("memory: CrossScopeQuery requires ScopeKind")
	}
	if len(q.Kinds) == 0 {
		return QueryResult{}, fmt.Errorf("memory: CrossScopeQuery requires at least one Kind")
	}
	return m.backend.QueryAllScopes(ctx, q)
}

// Search delegates to the configured Searcher. Returns
// ErrNoSearchProviders when no Searcher has been set.
//
// The per-kind read door applies here exactly as it does on Query, and for the
// same reason: a SearchProvider answers from the BACKEND rather than through
// this facade (the in-memory one calls Backend.Query directly; the SQL ones
// index every Kind that is Put), so nothing below this line re-applies it.
// Gating only Query would hide a Kind from the tool nobody calls while leaving
// it readable through search_memory, which is the tool the model holds.
func (m *Local) Search(ctx context.Context, req SearchRequest) (MergedSearchResult, error) {
	if m.searcher == nil {
		return MergedSearchResult{}, ErrNoSearchProviders
	}
	if err := m.authorizeKindRead(ctx, req.Kinds, "search"); err != nil {
		return MergedSearchResult{}, err
	}
	// The Search door lives in the CompositeSearcher (per-scope), so log a
	// capability denial here — the err text already names the denied perm+scope.
	res, err := m.searcher.Search(ctx, req)
	if err != nil && errors.Is(err, ErrMissingApproval) {
		m.logger.Info("memory: capability door denied — caller lacks a required approval (likely a missing-mint bug)",
			"op", "search", "err", err.Error())
	}
	if err != nil {
		return res, err
	}
	res.Entries = m.dropUnreadableScored(ctx, res.Entries, "search")
	return res, nil
}

// dropUnreadableScored is dropUnreadableKinds over ranked hits — the silent
// half of the door on the Search path, where a request naming no Kinds sweeps
// every Kind in the scope.
//
// PerProvider is deliberately left as each provider returned it: it is the raw
// diagnostic answer, it never reaches the model, and rewriting it would make
// "what did this provider actually match" unanswerable.
func (m *Local) dropUnreadableScored(ctx context.Context, entries []ScoredEntry, op string) []ScoredEntry {
	who, gated := kindReadDoorSubject(ctx)
	if !gated || len(entries) == 0 {
		return entries
	}
	out := entries[:0:0]
	dropped := 0
	for _, e := range entries {
		if SessionMayRead(e.Entry.Kind) {
			out = append(out, e)
			continue
		}
		dropped++
	}
	if dropped > 0 {
		m.logger.Info("memory: per-kind read door filtered platform-only entries from a search",
			"op", op, "dropped", dropped, "caller", who)
	}
	return out
}

// Status returns the backend's view of the scope's lifecycle state. No backend
// produces Archived today; runner-on-resume treats Live and Unknown identically.
func (m *Local) Status(ctx context.Context, scope Scope) (ScopeStatus, error) {
	return m.backend.Status(ctx, scope)
}

// Capabilities returns the backend's declared capabilities, for `oap
// doctor`-style tooling; correctness-side callers never branch on it.
func (m *Local) Capabilities() Capabilities { return m.backend.Capabilities() }

// SendSignal dispatches sig to every registered Kind's ScopeHooks for sig.Scope,
// materializing hooks on first dispatch per (Scope, Kind) and reusing them.
// Per-hook errors are collected via errors.Join, so a failing hook on one Kind
// never prevents other Kinds from reacting.
//
// The door: every call MUST carry an approval for (WriteMemory, sig.Scope.ID)
// (bearer, SpiceDB, or system) or SendSignal returns ErrMissingApproval before
// any hook is materialized or dispatched.
func (m *Local) SendSignal(ctx context.Context, sig Signal) error {
	if err := m.ensureApproval(ctx, WriteMemory, sig.Scope.ID, "signal"); err != nil {
		return err
	}
	// Authorship hands off here. A hook is operator-owned code, and the entries
	// it writes (the lifecycle timeline above all) are the OPERATOR's testimony
	// about a signal it received — signed as system:operator, not as the sender.
	// Leaving the sender's token identity on ctx would make the provenance
	// writer-binding demand that an operator-authored entry be attributed to the
	// session whose token triggered it, refusing every lifecycle entry a runner's
	// signal produces. Stripped HERE, at the hand-off rather than at the HTTP
	// boundary, so a future signal source gets the same treatment for free.
	ctx = WithoutTokenSession(ctx)
	hooks := m.hooksFor(sig.Scope)
	var errs []error
	for _, h := range hooks {
		if err := h.OnSignal(ctx, sig); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

const reindexMaxEntries = 50000

// Reindex re-Indexes every entry in scope on each registered SearchProvider,
// for adding a provider to an existing deployment or recovering from index
// corruption. Per-entry failures are logged and skipped; the count returned is
// the number of entries successfully submitted to ALL providers.
//
// The door: every call MUST carry an approval for (ReadMemory, scope.ID)
// (bearer, SpiceDB, or system) or Reindex returns ErrMissingApproval before
// touching the backend.
func (m *Local) Reindex(ctx context.Context, scope Scope) (int, error) {
	if err := m.ensureApproval(ctx, ReadMemory, scope.ID, "reindex"); err != nil {
		return 0, err
	}
	res, err := queryProbingTruncation(ctx, m.backend, Query{Scope: scope, Limit: reindexMaxEntries})
	if err != nil {
		return 0, fmt.Errorf("reindex: query: %w", err)
	}
	count := 0
	for _, e := range res.Entries {
		ok := true
		for _, sp := range m.search {
			if err := sp.Index(ctx, e.Scope, e.Kind, e.ID, e.Content); err != nil {
				m.logger.Info("reindex: index failed", "provider", sp.Name(), "entry", e.ID, "err", err.Error())
				ok = false
			}
		}
		if ok {
			count++
		}
	}
	if res.Truncated {
		m.logger.Info("reindex: hit entry limit, some entries may not have been reindexed", "scope", scope.ID, "limit", reindexMaxEntries)
	}
	return count, nil
}

// hooksFor returns the ordered list of ScopeHooks for every registered
// Kind, materializing per-(Scope, Kind) on first call. Ordering matches
// RegisteredKinds (sorted by Kind.Name()) for deterministic dispatch.
func (m *Local) hooksFor(scope Scope) []ScopeHooks {
	m.hooksMu.Lock()
	defer m.hooksMu.Unlock()
	kinds := RegisteredKinds()
	out := make([]ScopeHooks, 0, len(kinds))
	for _, k := range kinds {
		key := hooksKey{scopeKind: scope.Kind, scopeID: scope.ID, kindName: k.Name()}
		h, ok := m.hooks[key]
		if !ok {
			h = k.NewScopeHooks(scope)
			m.hooks[key] = h
		}
		out = append(out, h)
	}
	return out
}
