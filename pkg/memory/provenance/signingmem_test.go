package provenance_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

// flakyMemory fails the next Put when failNext is set, simulating the
// operator's verify-on-write rejecting an append (e.g. the publisher-key
// registry outage that 403'd every channelsd write). Other ops pass through.
type flakyMemory struct {
	memory.Memory
	failNext bool
}

func (f *flakyMemory) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	if f.failNext {
		return memory.Entry{}, errors.New("simulated verify-on-write rejection")
	}
	return f.Memory.Put(ctx, e)
}

// TestSigningMemory_RejectedPutDoesNotForkChain is the regression for the
// audit-chain corruption seen by `oap audit verify` (gap "missing seq 1" +
// "prevHash mismatch"). When an append-only Put is rejected, the signer had
// already advanced its in-memory chain head at Sign time and never rolled it
// back — so the next accepted write skipped a seq (gap) and linked its
// prevHash to a digest that was never stored (fork). The fix re-derives the
// head from durable storage after a failed Put, so the chain stays consistent
// with what actually landed.
func TestSigningMemory_RejectedPutDoesNotForkChain(t *testing.T) {
	registerKinds(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	const publisher = "system:channelsd"
	priv := testKey(9)
	signer := provenance.NewSigner(priv, publisher)

	inner := &flakyMemory{Memory: memory.NewLocal(inmem.NewBackend())}
	signed := provenance.NewSigningMemory(inner, signer)

	// 1. First append succeeds → seq 1.
	_, err := signed.Put(ctx, auditEntry("ta-a", `{"n":1}`))
	require.NoError(t, err)

	// 2. Second append is rejected (the outage). Sign already advanced the
	//    in-memory head; the fix must keep that advance from stranding the
	//    chain past the durable tail.
	inner.failNext = true
	_, err = signed.Put(ctx, auditEntry("ta-b", `{"n":2}`))
	require.Error(t, err, "a rejected Put surfaces the error")

	// 3. Third append succeeds — must continue from the last DURABLE entry
	//    (a, seq 1) as seq 2 linking to a, NOT seq 3 linking to the
	//    never-stored b.
	inner.failNext = false
	_, err = signed.Put(ctx, auditEntry("ta-c", `{"n":3}`))
	require.NoError(t, err)

	// 4. The durably-stored chain must verify clean: no gap, no fork.
	res, err := inner.Memory.Query(ctx, memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "ns/sess"},
		Kinds: []string{"test_audit"},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 2, "only the two accepted entries are stored (b was rejected)")

	verifier := provenance.NewVerifier(provenance.MapKeyLookup{{Publisher: publisher, KeyID: signer.KeyID()}: pub(priv)})
	report := verifier.VerifyChain(publisher, res.Entries, nil)
	assert.Empty(t, report.Findings, "stored chain must be gap-free and fork-free after a rejected Put")
	assert.Equal(t, 2, report.OK, "both stored entries verify")
}

// TestSigningMemory_IdempotentRePutDoesNotAdvanceChainHead is the regression
// for the other half of "a nil error does not prove our signed entry landed":
// memory.Local defines a byte-identical re-put of an append-only entry as
// IDEMPOTENT — it returns the already-stored entry, provenance untouched,
// with no error (entriesEquivalent deliberately excludes Provenance, precisely
// because "a retried write re-signs identical payload"). The seq Sign minted
// for that call is therefore never persisted. Before the fix, SigningMemory
// read the nil error as success and kept the advanced head, so the NEXT append
// skipped a seq (gap) and linked its PrevHash to a digest nothing stored
// (fork) — `oap audit verify` reporting tampering in an untampered log.
func TestSigningMemory_IdempotentRePutDoesNotAdvanceChainHead(t *testing.T) {
	registerKinds(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	const publisher = "system:channelsd"
	priv := testKey(17)
	signer := provenance.NewSigner(priv, publisher)
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}

	inner := memory.NewLocal(inmem.NewBackend())
	signed := provenance.NewSigningMemory(inner, signer)

	// 1. First append → seq 1, durably stored.
	first, err := signed.Put(ctx, auditEntry("ta-a", `{"n":1}`))
	require.NoError(t, err)
	require.NotNil(t, first.Provenance)
	require.Equal(t, uint64(1), first.Provenance.Seq, "first append is seq 1")

	// 2. Byte-identical re-put of the SAME entry (a retried write). The
	//    facade answers from what it already holds and stores nothing new,
	//    so the seq minted for this call never lands.
	again, err := signed.Put(ctx, auditEntry("ta-a", `{"n":1}`))
	require.NoError(t, err, "a byte-identical re-put of an append-only entry is idempotent, not an error")
	require.NotNil(t, again.Provenance)
	assert.Equal(t, uint64(1), again.Provenance.Seq,
		"the re-put returns the STORED entry, still at seq 1 — nothing new was persisted")

	// 3. A third, distinct append must continue from the last DURABLE entry
	//    (a, seq 1) as seq 2, NOT seq 3 behind the never-stored re-signing.
	_, err = signed.Put(ctx, auditEntry("ta-c", `{"n":3}`))
	require.NoError(t, err)

	// 4. The durably-stored chain must verify clean: no gap, no fork.
	res, err := inner.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"test_audit"}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 2, "the re-put added no entry")

	verifier := provenance.NewVerifier(provenance.MapKeyLookup{{Publisher: publisher, KeyID: signer.KeyID()}: pub(priv)})
	report := verifier.VerifyChain(publisher, res.Entries, nil)
	assert.Empty(t, report.Findings, "stored chain must be gap-free and fork-free after an idempotent re-put")
	assert.Equal(t, 2, report.OK, "both stored entries verify")
	assert.Equal(t, []uint64{1, 2}, seqsByID(res.Entries, "ta-a", "ta-c"),
		"the append after an idempotent re-put must take the next durable seq, not skip one")
}

// seqsByID returns the provenance Seq of each named entry, in the order the
// IDs are given, so a chain assertion reads as the expected sequence rather
// than as index arithmetic over an unordered query result. A missing entry or
// one with no provenance yields 0, which no valid chain position uses.
func seqsByID(entries []memory.Entry, ids ...string) []uint64 {
	byID := map[string]memory.Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if e, ok := byID[id]; ok && e.Provenance != nil {
			out = append(out, e.Provenance.Seq)
			continue
		}
		out = append(out, 0)
	}
	return out
}

// gateFlakyMemory blocks the Put whose entry ID equals blockedID until the
// test closes release, then fails it (simulating a rejected durable write —
// e.g. the operator's verify-on-write outage). It signals started as soon
// as the blocked Put is entered, so tests can force a specific interleaving
// deterministically instead of hoping goroutine scheduling reproduces a
// race. Puts for any other ID pass straight through.
type gateFlakyMemory struct {
	memory.Memory
	blockedID string
	started   chan struct{}
	release   chan struct{}
}

func (g *gateFlakyMemory) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	if e.ID == g.blockedID {
		close(g.started)
		<-g.release
		return memory.Entry{}, errors.New("simulated verify-on-write rejection")
	}
	return g.Memory.Put(ctx, e)
}

// auditEntryIn is auditEntry with an overridden Scope, for tests that need
// entries in more than one scope.
func auditEntryIn(scope memory.Scope, id, content string) memory.Entry {
	e := auditEntry(id, content)
	e.Scope = scope
	return e
}

// recvWithTimeout waits for ch with a bound, so a regression that
// reintroduces a deadlock (e.g. a single global lock instead of per-scope)
// fails fast instead of hanging until the test binary's own timeout.
func recvWithTimeout(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Put to complete")
		return nil
	}
}

// TestSigningMemory_ConcurrentAppendsSameScopeDoNotForkChain is the
// concurrent counterpart to TestSigningMemory_RejectedPutDoesNotForkChain
// above: instead of two sequential Puts in one goroutine, a second append
// races the first while it is still in flight. Before the per-scope lock in
// SigningMemory.Put, this reproduced the exact bug D-D2 describes: G1 signs
// seq 1 and blocks inside the durable Put; G2 (same scope) runs EnsureSeeded
// (already seeded — a no-op) then Sign, getting seq 2 stacked on G1's
// still-phantom seq 1; G1's Put is then rejected and InvalidateSeed fires,
// but G2's seq-2 entry — already built on top of the never-stored seq 1 —
// lands anyway. Durable result: seq 1 missing (gap) and seq 2's PrevHash
// points at a digest nothing stored (fork).
//
// The interleaving is forced deterministically via gateFlakyMemory rather
// than relying on goroutine-scheduling luck: by the time gate.started
// fires, G1 has unconditionally already run Sign (which happens-before the
// blocking Put call in the same goroutine), so G2 always observes the
// advanced chain head — this test does not flake either direction.
func TestSigningMemory_ConcurrentAppendsSameScopeDoNotForkChain(t *testing.T) {
	registerKinds(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	const publisher = "system:channelsd"
	priv := testKey(41)
	signer := provenance.NewSigner(priv, publisher)
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}

	inner := memory.NewLocal(inmem.NewBackend())
	gate := &gateFlakyMemory{
		Memory:    inner,
		blockedID: "ta-first",
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	signed := provenance.NewSigningMemory(gate, signer)

	// G1: signs seq 1, then blocks inside the durable Put until released.
	firstErr := make(chan error, 1)
	go func() {
		_, err := signed.Put(ctx, auditEntry("ta-first", `{"n":1}`))
		firstErr <- err
	}()
	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first append never reached the blocked durable Put")
	}

	// G2: a concurrent append to the SAME scope, launched while G1 is still
	// mid-flight. With the per-scope lock, this blocks on the scope's mutex
	// until G1 fully resolves (fails + InvalidateSeed) and releases it, so
	// G2 re-seeds from the (still-empty) durable store and signs seq 1
	// cleanly. Without the lock, G2 signs seq 2 on top of G1's phantom
	// seq 1, producing the gap+fork described above.
	secondErr := make(chan error, 1)
	go func() {
		_, err := signed.Put(ctx, auditEntry("ta-second", `{"n":2}`))
		secondErr <- err
	}()

	// Now let G1's blocked Put proceed — it fails, InvalidateSeed runs.
	close(gate.release)

	require.Error(t, recvWithTimeout(t, firstErr), "first append's durable Put is rejected, as designed")
	require.NoError(t, recvWithTimeout(t, secondErr), "second append must still land cleanly")

	res, err := inner.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"test_audit"}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1, "only the second entry lands durably; the first was rejected")

	verifier := provenance.NewVerifier(provenance.MapKeyLookup{{Publisher: publisher, KeyID: signer.KeyID()}: pub(priv)})
	report := verifier.VerifyChain(publisher, res.Entries, nil)
	assert.Empty(t, report.Findings, "a concurrent same-scope append racing a rejected append must not gap/fork the chain")
	assert.Equal(t, 1, report.OK)
	require.NotNil(t, res.Entries[0].Provenance)
	assert.Equal(t, uint64(1), res.Entries[0].Provenance.Seq,
		"the surviving entry must be re-seeded at seq 1, not stranded at seq 2 behind a phantom that was never stored")
}

// TestSigningMemory_ConcurrentDifferentScopesDoNotContend is the sanity
// check on the other side of the fix: per-scope locking must not regress
// into a single global lock. A Put blocked in scope A must not delay a
// concurrent Put to scope B.
func TestSigningMemory_ConcurrentDifferentScopesDoNotContend(t *testing.T) {
	registerKinds(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	signer := provenance.NewSigner(testKey(43), "system:channelsd")

	scopeA := memory.Scope{Kind: "session", ID: "ns/sess-a"}
	scopeB := memory.Scope{Kind: "session", ID: "ns/sess-b"}

	inner := memory.NewLocal(inmem.NewBackend())
	gate := &gateFlakyMemory{
		Memory:    inner,
		blockedID: "ta-a1",
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	signed := provenance.NewSigningMemory(gate, signer)

	goA := make(chan error, 1)
	go func() {
		_, err := signed.Put(ctx, auditEntryIn(scopeA, "ta-a1", `{"n":1}`))
		goA <- err
	}()
	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("scope A append never reached the blocked durable Put")
	}

	doneB := make(chan error, 1)
	go func() {
		_, err := signed.Put(ctx, auditEntryIn(scopeB, "ta-b1", `{"n":1}`))
		doneB <- err
	}()

	select {
	case err := <-doneB:
		require.NoError(t, err, "a different scope's Put must not block on scope A's lock")
	case <-time.After(2 * time.Second):
		t.Fatal("Put to a different scope blocked — per-scope locks must not serialize unrelated scopes")
	}

	close(gate.release)
	require.Error(t, recvWithTimeout(t, goA), "scope A's Put is rejected, as designed")
}

// reactiveHook Put()s a second append-only entry through target when it
// receives memory.SignalEntryAppended, and records what that Put returned.
// This is the exact shape signingmem.go's ErrEntryAppendedDispatchPut guards
// against: a ScopeHooks reaction appending back through the SAME
// SigningMemory that raised the signal. On the real dispatch path
// (memory.Local.Put) a hook's returned error is only logged, never
// propagated to the original caller — recording it here is what lets the
// test observe it.
type reactiveHook struct {
	target memory.Memory
	called bool
	gotErr error
}

func (h *reactiveHook) OnSignal(ctx context.Context, sig memory.Signal) error {
	if sig.Kind != memory.SignalEntryAppended {
		return nil
	}
	h.called = true
	_, h.gotErr = h.target.Put(ctx, memory.Entry{
		Scope:     sig.Scope,
		Kind:      "test_audit_reactive",
		ID:        "tr-reaction",
		CreatedAt: fixedTime,
		Content:   json.RawMessage(`{"reacted":true}`),
	})
	return h.gotErr
}

// entryAppendedReactiveKind is an append-only Kind whose ScopeHooks is a
// single shared reactiveHook, so the test can register it ahead of
// constructing the SigningMemory it will react through and wire hook.target
// in afterward.
type entryAppendedReactiveKind struct{ hook *reactiveHook }

func (entryAppendedReactiveKind) Name() string     { return "test_audit_reactive" }
func (entryAppendedReactiveKind) IDPrefix() string { return "tr-" }
func (entryAppendedReactiveKind) Retention() memory.Retention {
	return memory.Retention{AppendOnly: true}
}
func (entryAppendedReactiveKind) ContentSchema() reflect.Type { return nil }
func (entryAppendedReactiveKind) IndexedFields() []string     { return nil }
func (entryAppendedReactiveKind) WriteAuthority() memory.WriteAuthority {
	return memory.SessionWritten
}
func (k entryAppendedReactiveKind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return k.hook }

// TestSigningMemory_PutFromEntryAppendedDispatchIsRefused reproduces the real
// trigger end to end — a ScopeHooks reaction to SignalEntryAppended calling
// back into the SAME SigningMemory that raised the signal — rather than
// fabricating the ctx mark directly, since InEntryAppendedDispatch's setter
// (withEntryAppendedDispatch) is unexported to pkg/memory. It is the
// regression for the guard in Put returning a descriptive error instead of
// the silent `return e, nil` a caller could not tell apart from a genuine
// write.
func TestSigningMemory_PutFromEntryAppendedDispatchIsRefused(t *testing.T) {
	registerKinds(t)
	hook := &reactiveHook{}
	memory.RegisterKind(entryAppendedReactiveKind{hook: hook})

	ctx := memory.WithSystemApproval(context.Background(), "test")
	signer := provenance.NewSigner(testKey(61), "system:channelsd")

	inner := memory.NewLocal(inmem.NewBackend())
	signed := provenance.NewSigningMemory(inner, signer)
	hook.target = signed

	// Any append-only Put in this scope fans SignalEntryAppended out to every
	// registered Kind's ScopeHooks for the scope, including the reactive one
	// registered above — this is what actually drives ctx through
	// withEntryAppendedDispatch before the hook's own Put runs.
	_, err := signed.Put(ctx, auditEntry("ta-trigger", `{"n":1}`))
	require.NoError(t, err, "the triggering append itself must still succeed")

	require.True(t, hook.called, "the reactive hook must have run synchronously inside the triggering Put")
	require.Error(t, hook.gotErr, "a reactive Put from inside the entry-appended dispatch must be refused, not silently accepted")
	assert.ErrorIs(t, hook.gotErr, provenance.ErrEntryAppendedDispatchPut,
		"the refusal must be identifiable via errors.Is for a future ScopeHooks author who trips this guard")

	res, err := inner.Query(ctx, memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "ns/sess"},
		Kinds: []string{"test_audit_reactive"},
	})
	require.NoError(t, err)
	assert.Empty(t, res.Entries, "the refused Put must not have written anything")
}
