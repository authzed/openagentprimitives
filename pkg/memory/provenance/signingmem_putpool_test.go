// pkg/memory/provenance/signingmem_putpool_test.go — SigningMemory.PutToPool.
//
// This file pins the fix for a defect a green suite failed to notice: in a
// live runner, sess.Mem is a *SigningMemory wrapping an *httpclient.Client.
// SigningMemory embeds memory.Memory as an INTERFACE field, so Go's method
// promotion cannot forward the concrete Client's PutToPool — a type
// assertion for memory.PoolWriter against the OLD SigningMemory always
// failed, and record_observation refused every call in production while
// every test in the tree (built against a hand-rolled fake asserting the
// method exists, never against this actual composition) stayed green.
//
// It has now missed two further defects, both by the SAME mechanism, and the
// fix for the second is why this file no longer contains a fake server at all:
//
//   - A first version built its own input entry with an ID and a CreatedAt,
//     which no production caller of PutToPool set, and its fake server echoed
//     the body verbatim. record_observation therefore reached the wire with
//     ID:"" and a zero CreatedAt — both of which EntryDigest signs — and the
//     server assigning a real ID before verify-on-write meant the signature
//     could never re-verify, in every production call, invisibly to this test.
//   - The SECOND version fixed the POST half by replicating httpsrv.putEntry's
//     forcing and running the real VerifyEntrySignature — and left the GET half
//     stubbed with `200 {}` under a comment claiming "a brand-new pool answers
//     empty either way". Against the real server that is false: the pool read
//     EnsureSeeded issues had no route at all and answered 404, so the first
//     pool write failed outright in every mode. A lookalike fixture hid a hard
//     blocker, for the second time on this one test.
//
// So the fixture is gone. These tests run the PRODUCTION composition end to
// end over an httptest.Server carrying the REAL httpsrv handler, over a REAL
// memory.Local with a REAL provenance.WriteVerifier wired, reached through a
// REAL httpclient.Client. Nothing between the signer and the stored bytes is
// written by this file. That also closes the seam nothing in the branch
// exercised — an append-only PROVENANCE-VERIFIED write into a POOL scope,
// through the facade door rather than through a simulation of it — which is
// where two of this feature's failures lived.
package provenance_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observation"
	"github.com/authzed/openagentprimitives/pkg/memory/pools"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

const (
	poolTestNS   = "ns-a"
	poolTestSess = "sess-a"
	poolTestTok  = "tok-putpool-1"
)

// poolEntry builds an unsigned, unscoped append-only entry for the two
// fixture-Kind pool-write tests below (which do not exercise signing at all,
// or exercise the pass-through path) — SigningMemory.PutToPool stamps Scope
// before it ever reaches the wire, so it is not set here.
func poolEntry(id, content string) memory.Entry {
	return memory.Entry{Kind: "test_audit", ID: id, CreatedAt: fixedTime, Content: json.RawMessage(content)}
}

// registerObservationKind registers the REAL observation Kind for the
// duration of the test, the same wipe-to-empty-then-restore discipline
// registerKinds uses for its synthetic fixtures — applied here to the actual
// Kind this feature writes, so memory.KindAppendOnly("observation") answers
// true exactly as it does in production. Needed because registerKinds (and
// its cleanup) resets the GLOBAL Kind registry to empty, which would
// otherwise silently drop "observation" (registered by that package's own
// init()) for any test that shares this process with one that called
// registerKinds first.
func registerObservationKind(t *testing.T) {
	t.Helper()
	memory.ResetRegistryForTest()
	memory.RegisterKind(observation.Kind{})
	t.Cleanup(memory.ResetRegistryForTest)
}

// grantedPools is a pools.RelationshipReader emitting write-slot grants for
// the session under test. It is the ONLY thing standing between this session
// and a pool, so the refusal case below drives the same reader with a
// different grant set rather than a different door.
type grantedPools struct{ writeGrants []string }

func (g grantedPools) ReadRelationships(_ context.Context, _ pools.RelFilter) ([]authz.Relation, error) {
	sess := authz.SessionRef{Namespace: poolTestNS, Name: poolTestSess}
	var rels []authz.Relation
	for _, ref := range g.writeGrants {
		objType, objID, _ := strings.Cut(ref, ":")
		rels = append(rels, authz.SlotGrantRelation(objType, objID, "write_memory", sess))
	}
	return rels, nil
}

// realMemoryAPI stands up the production server stack: a memory.Local over an
// inmem backend with verify-on-write ENABLED (so an unsigned or forged
// append-only write is refused at the facade door, exactly as in the operator),
// behind the real httpsrv handler with the real per-session token registry and
// the real WithPools option.
//
// Returns the client the runner would hold, plus the facade itself so a test
// can read what actually landed without going back through the wire.
func realMemoryAPI(t *testing.T, keys provenance.PublisherKeyLookup, gp grantedPools) (*httpclient.Client, *memory.Local) {
	t.Helper()
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: poolTestNS, Name: poolTestSess}, poolTestTok, "")
	local := memory.NewLocal(inmem.NewBackend(),
		memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	srv := httptest.NewServer(httpsrv.NewHandler(local, reg, httpsrv.WithPools(gp)))
	t.Cleanup(srv.Close)
	return httpclient.New(srv.URL, poolTestTok), local
}

// sessionSigner builds the signer the runner builds: keyed to the SESSION's own
// publisher, because WriteVerifier.checkAuthor binds a per-session bearer to
// exactly that publisher and refuses anything else. A "system:runner" publisher
// would be refused by the real door — which is precisely the kind of thing a
// fake server could not have told us.
func sessionSigner(t *testing.T, seed byte) (*provenance.Signer, provenance.MapKeyLookup) {
	t.Helper()
	priv := testKey(seed)
	s := provenance.NewSigner(priv, provenance.SessionPublisher(poolTestNS, poolTestSess))
	return s, provenance.MapKeyLookup{{Publisher: s.Publisher(), KeyID: s.KeyID()}: pub(priv)}
}

// Pool writes must reject a missing timestamp before consuming a position too.
func TestSigningMemory_PutToPool_ZeroTimestampDoesNotConsumeChainPosition(t *testing.T) {
	registerObservationKind(t)
	signer, keys := sessionSigner(t, 0x09)
	client, _ := realMemoryAPI(t, keys, grantedPools{writeGrants: []string{"observation_dossier:case-timestamps"}})
	signed := provenance.NewSigningMemory(client, signer)
	pool := memory.Scope{Kind: "resource", ID: "observation_dossier:case-timestamps"}
	session := memory.Scope{Kind: "session", ID: poolTestNS + "/" + poolTestSess}
	entry := memory.Entry{Kind: observation.KindName, ID: "obs-zero", Content: json.RawMessage(`{"summary":"test"}`)}
	_, err := signed.PutToPool(context.Background(), session, pool, entry)
	require.ErrorContains(t, err, "zero CreatedAt")
	entry.ID = "obs-next"
	entry.CreatedAt = time.Now().UTC()
	stored, err := signed.PutToPool(context.Background(), session, pool, entry)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), stored.Provenance.Seq)
}

// TestSigningMemory_PutToPool_SignsAndLandsThroughTheRealMemoryAPI is the test
// that matters most in this file: the ACTUAL production composition — a
// *provenance.SigningMemory over an *httpclient.Client (what
// internal/cmd/runner builds) talking to the real httpsrv handler over a real
// verifying memory.Local — writing a real observation into a real pool scope.
func TestSigningMemory_PutToPool_SignsAndLandsThroughTheRealMemoryAPI(t *testing.T) {
	registerObservationKind(t)
	ctx := context.Background()

	poolScope, err := memory.ResourceScope("dossier", "d-1")
	require.NoError(t, err)
	sessionScope := memory.Scope{Kind: "session", ID: poolTestNS + "/" + poolTestSess}

	signer, keys := sessionSigner(t, 21)
	client, local := realMemoryAPI(t, keys, grantedPools{writeGrants: []string{"dossier:d-1"}})
	signed := provenance.NewSigningMemory(client, signer)

	entry1, merr := observation.NewEntry("prefers async review", []string{"process"})
	require.NoError(t, merr)
	stored, err := signed.PutToPool(ctx, sessionScope, poolScope, entry1)
	require.NoError(t, err,
		"the FIRST pool write over the production composition must land — the chain-seeding read it performs "+
			"had no route to a pool and answered 404, which failed every call in every mode")
	require.NotNil(t, stored.Provenance)
	assert.Equal(t, uint64(1), stored.Provenance.Seq, "first write into this pool opens the chain at seq 1")

	// What LANDED, read from the facade rather than from the response: the
	// entry is in the POOL's scope, signed, and independently verifiable.
	sysCtx := memory.WithSystemApproval(ctx, "test")
	inPool, qerr := local.Query(sysCtx, memory.Query{Scope: poolScope, Kinds: []string{observation.KindName}})
	require.NoError(t, qerr)
	require.Len(t, inPool.Entries, 1, "the observation must be in the POOL, not the session's own scope")
	first := inPool.Entries[0]
	assert.Equal(t, poolScope, first.Scope)
	assert.Equal(t, signer.Publisher(), first.Provenance.Publisher)
	require.NoError(t, provenance.VerifyEntrySignature(keys, first),
		"the entry as the REAL facade stored it must verify — the server forces Scope/Kind/ID, so a digest "+
			"signed over anything else could never re-verify against what landed")

	own, qerr := local.Query(sysCtx, memory.Query{Scope: sessionScope, Kinds: []string{observation.KindName}})
	require.NoError(t, qerr)
	assert.Empty(t, own.Entries, "nothing may be written into the session's own scope by a pool write")

	// A second write continues the chain rather than restarting it.
	entry2, merr := observation.NewEntry("a second, unrelated note", nil)
	require.NoError(t, merr)
	_, err = signed.PutToPool(ctx, sessionScope, poolScope, entry2)
	require.NoError(t, err)

	inPool, qerr = local.Query(sysCtx, memory.Query{Scope: poolScope, Kinds: []string{observation.KindName}})
	require.NoError(t, qerr)
	require.Len(t, inPool.Entries, 2)

	// Verify with the SAME machinery `oap audit verify` uses, over the entries
	// in chain order, rather than hand-recomputing a digest: a genuine gap or
	// fork fails this the way it would fail in production.
	chain := entriesBySeq(t, inPool.Entries)
	report := provenance.NewVerifier(keys).VerifyChain(signer.Publisher(), chain, nil)
	assert.Empty(t, report.Findings, "the two pool-scoped writes must form one gap-free, fork-free chain")
	assert.Equal(t, 2, report.OK)
}

// TestSigningMemory_PutToPool_AFreshSignerResumesTheChainFromThePool is the
// restart case, and the direct pin on the pool-aware READ route.
//
// A second Signer over the same key — a runner that restarted — must seed its
// chain head from the pool's existing entries and continue at seq 3, not
// re-open at seq 1. That seeding read is a query against a RESOURCE scope over
// an HTTP client, which is the addressing that did not exist. Without it this
// test cannot even reach an assertion: the write errors.
func TestSigningMemory_PutToPool_AFreshSignerResumesTheChainFromThePool(t *testing.T) {
	registerObservationKind(t)
	ctx := context.Background()

	poolScope, err := memory.ResourceScope("dossier", "d-1")
	require.NoError(t, err)
	sessionScope := memory.Scope{Kind: "session", ID: poolTestNS + "/" + poolTestSess}

	signer, keys := sessionSigner(t, 24)
	client, local := realMemoryAPI(t, keys, grantedPools{writeGrants: []string{"dossier:d-1"}})

	for _, text := range []string{"first", "second"} {
		e, merr := observation.NewEntry(text, nil)
		require.NoError(t, merr)
		_, err = provenance.NewSigningMemory(client, signer).PutToPool(ctx, sessionScope, poolScope, e)
		require.NoError(t, err)
	}

	// The restart: a brand-new Signer, same key, no in-memory chain state.
	resumed, _ := sessionSigner(t, 24)
	e3, merr := observation.NewEntry("after the restart", nil)
	require.NoError(t, merr)
	out, err := provenance.NewSigningMemory(client, resumed).PutToPool(ctx, sessionScope, poolScope, e3)
	require.NoError(t, err)
	require.NotNil(t, out.Provenance)
	assert.Equal(t, uint64(3), out.Provenance.Seq,
		"a restarted publisher must RESUME the pool's chain, not open a second segment at seq 1 — which is "+
			"what it does when the seeding read cannot reach the pool")

	sysCtx := memory.WithSystemApproval(ctx, "test")
	inPool, qerr := local.Query(sysCtx, memory.Query{Scope: poolScope, Kinds: []string{observation.KindName}})
	require.NoError(t, qerr)
	require.Len(t, inPool.Entries, 3)
	report := provenance.NewVerifier(keys).VerifyChain(resumed.Publisher(), entriesBySeq(t, inPool.Entries), nil)
	assert.Empty(t, report.Findings, "all three entries must form ONE chain across the restart")
	assert.Equal(t, 3, report.OK)
}

// TestSigningMemory_PutToPool_AnUngrantedPoolIsRefusedByTheRealDoor keeps the
// happy path above from being vacuous. The same composition, the same write,
// one difference — the session holds no write grant on the destination — and
// the real server must refuse it with nothing written anywhere.
func TestSigningMemory_PutToPool_AnUngrantedPoolIsRefusedByTheRealDoor(t *testing.T) {
	registerObservationKind(t)
	ctx := context.Background()

	poolScope, err := memory.ResourceScope("dossier", "d-1")
	require.NoError(t, err)
	sessionScope := memory.Scope{Kind: "session", ID: poolTestNS + "/" + poolTestSess}

	signer, keys := sessionSigner(t, 25)
	// A grant on a DIFFERENT resource: held, but not this one.
	client, local := realMemoryAPI(t, keys, grantedPools{writeGrants: []string{"dossier:other"}})
	signed := provenance.NewSigningMemory(client, signer)

	e, merr := observation.NewEntry("prefers async review", nil)
	require.NoError(t, merr)
	_, err = signed.PutToPool(ctx, sessionScope, poolScope, e)
	require.Error(t, err, "a pool this session holds no write grant on must be refused")
	assert.Contains(t, err.Error(), "no write grant")

	sysCtx := memory.WithSystemApproval(ctx, "test")
	for _, sc := range []memory.Scope{poolScope, sessionScope} {
		res, qerr := local.Query(sysCtx, memory.Query{Scope: sc, Kinds: []string{observation.KindName}})
		require.NoError(t, qerr)
		assert.Empty(t, res.Entries,
			"a refused pool write must land nowhere — a silent downgrade to the session scope is the failure this gate exists to prevent (scope %s)", sc.ID)
	}
}

// TestSigningMemory_PutToPool_AnUnsignedAppendOnlyWriteIsRefusedByTheFacade
// drives the append-only facade DOOR for a pool write directly, which no test
// in this feature did: the e2e posts unsigned entries through a harness with no
// verifier wired, and the composition test used to simulate verification in a
// fake. Two of this feature's failures lived in exactly this seam.
//
// Writing through the bare client (no signing wrapper) is the shape of a caller
// that forgot to sign. The real verifier must refuse it, in the POOL scope, the
// same way it refuses one in a session scope.
func TestSigningMemory_PutToPool_AnUnsignedAppendOnlyWriteIsRefusedByTheFacade(t *testing.T) {
	registerObservationKind(t)
	ctx := context.Background()

	poolScope, err := memory.ResourceScope("dossier", "d-1")
	require.NoError(t, err)
	sessionScope := memory.Scope{Kind: "session", ID: poolTestNS + "/" + poolTestSess}

	_, keys := sessionSigner(t, 26)
	client, local := realMemoryAPI(t, keys, grantedPools{writeGrants: []string{"dossier:d-1"}})

	e, merr := observation.NewEntry("unsigned, straight off the client", nil)
	require.NoError(t, merr)
	_, err = client.PutToPool(ctx, sessionScope, poolScope, e)
	require.Error(t, err, "an append-only write with no provenance must be refused at the facade door")

	sysCtx := memory.WithSystemApproval(ctx, "test")
	res, qerr := local.Query(sysCtx, memory.Query{Scope: poolScope, Kinds: []string{observation.KindName}})
	require.NoError(t, qerr)
	assert.Empty(t, res.Entries, "nothing may land when verification refuses")
}

// TestSigningMemory_PutToPool_FailsLoudlyWhenTheInnerCannotWritePools pins
// the "fail loudly, name the concrete type" requirement: a *memory.Local
// (the operator's in-process facade) implements memory.Memory but not
// memory.PoolWriter, so wrapping it must refuse with a clear, actionable
// error — never a panic, and never silence that reads the same as "no grant".
//
// This never reaches the append-only signing path at all (the PoolWriter
// assertion fails first), so the fixture Kind used here is irrelevant to
// what the test proves; "test_audit" via registerKinds is fine.
func TestSigningMemory_PutToPool_FailsLoudlyWhenTheInnerCannotWritePools(t *testing.T) {
	registerKinds(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	poolScope, err := memory.ResourceScope("dossier", "d-1")
	require.NoError(t, err)

	inner := memory.NewLocal(inmem.NewBackend())
	signer := provenance.NewSigner(testKey(22), "system:runner")
	signed := provenance.NewSigningMemory(inner, signer)

	_, err = signed.PutToPool(ctx, memory.Scope{Kind: "session", ID: "ns/sess"}, poolScope,
		poolEntry("ta-x", `{"n":1}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not implement memory.PoolWriter",
		"an operator reading this must learn WHICH capability is missing, not just that the call failed")
	assert.Contains(t, err.Error(), "memory.Local",
		"names the concrete inner type, so an operator knows exactly which layer to fix")

	res, qerr := inner.Query(ctx, memory.Query{Scope: poolScope, Kinds: []string{"test_audit"}})
	require.NoError(t, qerr)
	assert.Empty(t, res.Entries, "nothing may be written when the inner cannot honour the destination")
}

// TestSigningMemory_PutToPool_MutableKindPassesThroughUnsigned mirrors Put's
// own pass-through for a non-append-only Kind: forwarded, scoped, but never
// signed.
func TestSigningMemory_PutToPool_MutableKindPassesThroughUnsigned(t *testing.T) {
	registerKinds(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	poolScope, err := memory.ResourceScope("dossier", "d-1")
	require.NoError(t, err)

	var gotEntry memory.Entry
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotEntry))
		require.NoError(t, json.NewEncoder(w).Encode(gotEntry))
	}))
	t.Cleanup(srv.Close)

	client := httpclient.New(srv.URL, "tok")
	signer := provenance.NewSigner(testKey(23), "system:runner")
	signed := provenance.NewSigningMemory(client, signer)

	_, err = signed.PutToPool(ctx, memory.Scope{Kind: "session", ID: "ns/sess"}, poolScope,
		memory.Entry{Kind: "test_mutable", ID: "tm-1", CreatedAt: fixedTime, Content: json.RawMessage(`{"n":1}`)})
	require.NoError(t, err)

	assert.Nil(t, gotEntry.Provenance, "a mutable Kind must never be signed")
	assert.Equal(t, poolScope, gotEntry.Scope, "the destination is still stamped even when unsigned")
}

// entriesBySeq orders entries by their provenance sequence, which is the order
// VerifyChain expects. The facade returns newest-first, and a chain verified in
// the wrong order reports fabricated findings that look exactly like the real
// ones — so the ordering is done here, explicitly, rather than assumed.
func entriesBySeq(t *testing.T, in []memory.Entry) []memory.Entry {
	t.Helper()
	out := make([]memory.Entry, len(in))
	for _, e := range in {
		require.NotNil(t, e.Provenance, "every append-only entry must carry provenance")
		idx := int(e.Provenance.Seq) - 1
		require.GreaterOrEqual(t, idx, 0)
		require.Less(t, idx, len(out), "seq %d is outside the %d entries read back", e.Provenance.Seq, len(out))
		out[idx] = e
	}
	return out
}
