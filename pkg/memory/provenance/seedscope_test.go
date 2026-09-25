package provenance_test

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

// secondAuditKind is a second append-only Kind, so "every append-only kind" is
// a claim a single-kind registry could not distinguish from "the one kind".
type secondAuditKind struct{}

func (secondAuditKind) Name() string                                 { return "test_audit_two" }
func (secondAuditKind) IDPrefix() string                             { return "tb-" }
func (secondAuditKind) Retention() memory.Retention                  { return memory.Retention{AppendOnly: true} }
func (secondAuditKind) ContentSchema() reflect.Type                  { return nil }
func (secondAuditKind) IndexedFields() []string                      { return nil }
func (secondAuditKind) WriteAuthority() memory.WriteAuthority        { return memory.SessionWritten }
func (secondAuditKind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return noopHooks{} }

// queryRecorder records the Kinds each Query asked for.
type queryRecorder struct {
	memory.Memory
	kinds []string
}

func (q *queryRecorder) Query(ctx context.Context, qy memory.Query) (memory.QueryResult, error) {
	q.kinds = append(q.kinds, qy.Kinds...)
	return q.Memory.Query(ctx, qy)
}

// Chain seeding scans EVERY registered append-only Kind and no mutable one,
// because the chain head is "the highest seq this publisher signed anywhere in
// this scope" and a Kind left out of the scan under-counts it — the next append
// then reuses a seq (gap) and links to a digest that is not the tail (fork),
// which `oap audit verify` cannot tell from tampering.
//
// This is the tamper-evident log's own bookkeeping and belongs to no feature.
// It is worth pinning because the scan's error message names whichever Kind it
// reached first — `approval` in a real registry, since RegisteredKinds is
// sorted by name — which reads as a permission check and is not one. Narrowing
// the scan to "the Kinds some feature needs" silently weakens the log.
func TestSeedFromMemory_scansEveryRegisteredAppendOnlyKindAndNoMutableOne(t *testing.T) {
	memory.ResetRegistryForTest()
	memory.RegisterKind(auditKind{})
	memory.RegisterKind(secondAuditKind{})
	memory.RegisterKind(mutKind{})
	t.Cleanup(memory.ResetRegistryForTest)

	ctx := memory.WithSystemApproval(context.Background(), "test")
	rec := &queryRecorder{Memory: memory.NewLocal(inmem.NewBackend())}
	signer := provenance.NewSigner(testKey(7), "system:operator")

	require.NoError(t, signer.SeedFromMemory(ctx, rec, memory.Scope{Kind: "session", ID: "ns/sess"}))

	sort.Strings(rec.kinds)
	assert.Equal(t, []string{"test_audit", "test_audit_two"}, rec.kinds,
		"every append-only Kind is scanned; a mutable Kind carries no chain and is not")
}

// zReadableKind is a third append-only Kind sorting AFTER the unreadable one, so
// "the seed CONTINUES past a kind it may not read" is distinguishable from "the
// seed stopped at that kind" — the latter would never reach this one.
type zReadableKind struct{}

func (zReadableKind) Name() string                                 { return "test_zreadable" }
func (zReadableKind) IDPrefix() string                             { return "tz-" }
func (zReadableKind) Retention() memory.Retention                  { return memory.Retention{AppendOnly: true} }
func (zReadableKind) ContentSchema() reflect.Type                  { return nil }
func (zReadableKind) IndexedFields() []string                      { return nil }
func (zReadableKind) WriteAuthority() memory.WriteAuthority        { return memory.SessionWritten }
func (zReadableKind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return noopHooks{} }

// unreadableKind is a registered append-only Kind a SESSION credential may not
// read — the shape of pt_tag_content, the per-datum content-binding ledger. A
// session's chain-seed scan names it explicitly and the facade's read door
// refuses it (ErrKindNotSessionReadable).
type unreadableKind struct{}

func (unreadableKind) Name() string                                 { return "test_unreadable" }
func (unreadableKind) IDPrefix() string                             { return "ur-" }
func (unreadableKind) Retention() memory.Retention                  { return memory.Retention{AppendOnly: true} }
func (unreadableKind) ContentSchema() reflect.Type                  { return nil }
func (unreadableKind) IndexedFields() []string                      { return nil }
func (unreadableKind) WriteAuthority() memory.WriteAuthority        { return memory.SessionWritten }
func (unreadableKind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return noopHooks{} }
func (unreadableKind) SessionReadable() bool                        { return false }

// recordingRefuser records the kinds queried and refuses the unreadable one with
// the exact error the facade's read door raises for a session credential, so the
// seed exercises its real skip path without standing up a token session.
type recordingRefuser struct {
	memory.Memory
	queried []string
}

func (m *recordingRefuser) Query(ctx context.Context, qy memory.Query) (memory.QueryResult, error) {
	m.queried = append(m.queried, qy.Kinds...)
	for _, k := range qy.Kinds {
		if k == "test_unreadable" {
			return memory.QueryResult{}, fmt.Errorf("%w: %q is not session-readable", memory.ErrKindNotSessionReadable, k)
		}
	}
	return m.Memory.Query(ctx, qy)
}

// TestSeedFromMemory_skipsKindsTheCredentialCannotRead pins the fix for the
// per-datum-egress live-session boot failure: a session's provenance chain-seed
// scans EVERY append-only kind for this publisher's chain head, but pt_tag_content
// is SessionReadable()=false by design, so the facade's read door refused it and
// the whole seed failed with MemoryUnavailable — the session could not start. A
// kind the reader cannot read holds no chain this publisher can resume (it cannot
// even see its own entries there), so the seed must SKIP it and continue, not
// abort. Platform credentials, which CAN read pt_tag_content, still scan it and
// anchor its chain — the skip is keyed on the read-door error, which only a
// session credential provokes.
func TestSeedFromMemory_skipsKindsTheCredentialCannotRead(t *testing.T) {
	memory.ResetRegistryForTest()
	memory.RegisterKind(auditKind{})      // test_audit    (readable, sorts first)
	memory.RegisterKind(unreadableKind{}) // test_unreadable (refused)
	memory.RegisterKind(zReadableKind{})  // test_zreadable (readable, sorts last)
	t.Cleanup(memory.ResetRegistryForTest)

	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := &recordingRefuser{Memory: memory.NewLocal(inmem.NewBackend())}
	signer := provenance.NewSigner(testKey(9), "session:ns/sess")

	err := signer.SeedFromMemory(ctx, mem, memory.Scope{Kind: "session", ID: "ns/sess"})
	require.NoError(t, err,
		"a kind the credential cannot read (pt_tag_content) must be SKIPPED in the chain seed, not fail the whole seed")
	assert.Contains(t, mem.queried, "test_zreadable",
		"the seed must CONTINUE past the unreadable kind to later readable kinds, not stop at the refusal")
}

// The scan needs a readable scope. The capability door denies a caller that
// minted nothing, so an append-only write from such a caller fails BEFORE it
// reaches the store — which is what wedged a tool call whose only symptom was
// a query naming an unrelated Kind.
func TestSigningMemory_appendOnlyPutNeedsAReadableScopeToSeedTheChain(t *testing.T) {
	registerKinds(t)
	signed := provenance.NewSigningMemory(
		memory.NewLocal(inmem.NewBackend()), provenance.NewSigner(testKey(8), "system:operator"))

	_, err := signed.Put(context.Background(), auditEntry("ta-x", `{"n":1}`))
	require.Error(t, err, "no approval minted: the door denies the seeding read")
	assert.ErrorIs(t, err, memory.ErrMissingApproval)
	assert.Contains(t, err.Error(), "chain head",
		"the message has to say the kind is being SCANNED for a chain head; naming the kind alone reads as a permission the caller was asked for")

	_, err = signed.Put(memory.WithSystemApproval(context.Background(), "test"), auditEntry("ta-x", `{"n":1}`))
	assert.NoError(t, err, "with the mint the same write lands")
}
