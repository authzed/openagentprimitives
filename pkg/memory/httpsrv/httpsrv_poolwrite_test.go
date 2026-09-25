// pkg/memory/httpsrv/httpsrv_poolwrite_test.go — the slot gate on the write
// door.
//
// A session reaches a resource's memory pool through a slot grant, and this
// file pins the ONE thing standing between a session and a pool it holds no
// grant on: the proof that the destination named on the request line is a pool
// the session provably holds a WRITE grant on.
//
// The gate is not mode-sensitive — no information-leakage setting, no read
// door, no per-kind authority reaches it — so if it is wrong, nothing else
// catches it. That is why the refusal cases outnumber the happy path here.
//
// Two properties every test below is arranged around:
//
//   - The BODY stays untrusted for scope. putEntry still forces e.Scope from a
//     server-decided value; what changed is only which server-decided value.
//     TestABodySuppliedScopeIsStillIgnored pins that a body naming a pool the
//     session CAN write is still ignored — the grant is not what makes the body
//     trustworthy, the request line is.
//   - A pool the session holds no write grant on is REFUSED, never quietly
//     downgraded to the session scope. A silent downgrade writes the data
//     somewhere plausible and hides the broken grant, which a human would not
//     notice until the entry was needed and missing —
//     TestAnUngrantedPoolIsRefusedRatherThanDowngradedToTheSession is the
//     assertion that a downgrade cannot pass.
//
// package httpsrv (white-box), like httpsrv_pools_test.go: the harness builds
// *handler directly so it controls mem, reg and pools without a k8s client or
// an artifact store.
package httpsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observation"
	"github.com/authzed/openagentprimitives/pkg/memory/pools"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

const (
	poolWriteNS   = "ns-a"
	poolWriteSess = "sess-a"
	poolWriteTok  = "tok-poolwrite-1"
)

// stubPools is a pools.RelationshipReader that emits slot grants for the
// session under test. It is written in terms of the DIRECTION a test cares
// about rather than raw tuples, because the read/write asymmetry is the thing
// being pinned: any slot_grant_* makes a read pool, only
// slot_grant_write_memory makes a write pool (pools.ForSession), so a
// readGrants entry is exactly the "held, but not writable" case the gate must
// refuse.
type stubPools struct {
	// writeGrants are "<type>:<id>" refs held under a write_memory slot.
	writeGrants []string
	// readGrants are "<type>:<id>" refs held under an ordinary (read-only) slot.
	readGrants []string
	// err makes grant discovery fail, so a test can pin the fail-closed branch:
	// a grant set that could not be READ is unknown, not empty.
	err error
}

func (s *stubPools) ReadRelationships(_ context.Context, _ pools.RelFilter) ([]authz.Relation, error) {
	if s.err != nil {
		return nil, s.err
	}
	sess := authz.SessionRef{Namespace: poolWriteNS, Name: poolWriteSess}
	var rels []authz.Relation
	for _, ref := range s.writeGrants {
		objType, objID, _ := strings.Cut(ref, ":")
		rels = append(rels, authz.SlotGrantRelation(objType, objID, "write_memory", sess))
	}
	for _, ref := range s.readGrants {
		objType, objID, _ := strings.Cut(ref, ":")
		rels = append(rels, authz.SlotGrantRelation(objType, objID, "read", sess))
	}
	return rels, nil
}

// newHandlerWithPools wires a real *memory.Local over an inmem backend and a
// registry holding the one per-session bearer these tests present, then
// installs sp through WithPools — the production option, so the typed-nil
// normalization it performs is on the path under test rather than bypassed.
//
// sp is taken BY VALUE and its address stored, so a caller writes the literal
// the test reads best (stubPools{writeGrants: …}) without having to take an
// address at every call site.
func newHandlerWithPools(t *testing.T, sp stubPools) *handler {
	t.Helper()
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: poolWriteNS, Name: poolWriteSess}, poolWriteTok, "")
	h := &handler{mem: memory.NewLocal(meminmem.NewBackend()), reg: reg}
	WithPools(&sp)(h)
	return h
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err, "marshal content")
	return b
}

// postEntryRaw issues the write and returns the raw status + body, for the
// refusal cases: the STATUS is the assertion, so it must not be swallowed by a
// require inside a helper.
func postEntryRaw(t *testing.T, h *handler, ns, name, kind, query string, e memory.Entry) (int, string) {
	t.Helper()
	body, err := json.Marshal(e)
	require.NoError(t, err, "marshal Entry")
	req := httptest.NewRequest(http.MethodPost, "/memory/"+kind+"/"+ns+"/"+name+query, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+poolWriteTok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// postEntry is postEntryRaw for the paths that must succeed: it requires a 201
// and returns the STORED entry, whose Scope is what every happy-path assertion
// here is about.
func postEntry(t *testing.T, h *handler, ns, name, kind, query string, e memory.Entry) memory.Entry {
	t.Helper()
	code, body := postEntryRaw(t, h, ns, name, kind, query, e)
	require.Equal(t, http.StatusCreated, code, "POST must succeed; body=%s", body)
	var got memory.Entry
	require.NoError(t, json.Unmarshal([]byte(body), &got), "decode stored Entry")
	return got
}

// getEntriesRaw issues the keyed route's GET and returns the raw status +
// body, for the listing cases where the STATUS is the assertion. query is
// appended to the URL VERBATIM, so a test can send a query string Go's own
// parser rejects.
func getEntriesRaw(t *testing.T, h *handler, ns, name, kind, query string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/memory/"+kind+"/"+ns+"/"+name+query, nil)
	req.Header.Set("Authorization", "Bearer "+poolWriteTok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// listEntries requires a 200 and returns what the listing held.
func listEntries(t *testing.T, h *handler, ns, name, kind, query string) []memory.Entry {
	t.Helper()
	code, body := getEntriesRaw(t, h, ns, name, kind, query)
	require.Equal(t, http.StatusOK, code, "GET must succeed; body=%s", body)
	var res memory.QueryResult
	require.NoError(t, json.Unmarshal([]byte(body), &res), "decode QueryResult")
	return res.Entries
}

// listScope lists the SESSION scope's entries of a Kind, so a test can prove a
// refused write landed NOWHERE rather than merely not landing in the pool.
func listScope(t *testing.T, h *handler, ns, name, kind string) []memory.Entry {
	t.Helper()
	return listEntries(t, h, ns, name, kind, "")
}

// texts returns each entry's observation text, for asserting on WHICH entries
// a listing held rather than on server-minted ids.
func texts(t *testing.T, entries []memory.Entry) []string {
	t.Helper()
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		var c observation.Content
		require.NoError(t, json.Unmarshal(e.Content, &c), "decode observation content")
		out = append(out, c.Text)
	}
	return out
}

// A session writes into a pool it holds a write grant on.
func TestPoolWriteLandsInTheNamedPoolWhenTheSessionHoldsTheGrant(t *testing.T) {
	h := newHandlerWithPools(t, stubPools{writeGrants: []string{"dossier:d-1"}})

	got := postEntry(t, h, poolWriteNS, poolWriteSess, observation.KindName, "?pool=dossier:d-1",
		memory.Entry{Content: mustJSON(t, observation.Content{Text: "prefers async review"})})

	assert.True(t, memory.IsResourceScope(got.Scope))
	assert.Equal(t, "resource", got.Scope.Kind)
	assert.Equal(t, "dossier:d-1", got.Scope.ID)
	// ONLY in the pool. A write that landed in both would satisfy every
	// assertion above while quietly duplicating a session's observations into
	// its own scope.
	assert.Empty(t, listScope(t, h, poolWriteNS, poolWriteSess, observation.KindName),
		"a pool write must land in the pool alone, not in the pool and the session")
}

// The gate: a pool the session holds NO write grant on is refused.
func TestPoolWriteIsRefusedWithoutAWriteGrant(t *testing.T) {
	h := newHandlerWithPools(t, stubPools{writeGrants: []string{"dossier:d-1"}})

	code, body := postEntryRaw(t, h, poolWriteNS, poolWriteSess, observation.KindName, "?pool=ledger:l-9",
		memory.Entry{Content: mustJSON(t, observation.Content{Text: "x"})})

	assert.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "ledger:l-9")
	assert.Contains(t, body, "write")
}

// Holding a pool is not the same as being allowed to write into it. A
// read-only slot makes the resource a READ pool (pools.ForSession puts any
// slot_grant_* in Read), and writing into it is an egress the read grant never
// authorized — so the gate must prove against Write, not against "held".
func TestAReadOnlySlotDoesNotAuthorizeWritingThePool(t *testing.T) {
	h := newHandlerWithPools(t, stubPools{readGrants: []string{"dossier:d-1"}})

	code, body := postEntryRaw(t, h, poolWriteNS, poolWriteSess, observation.KindName, "?pool=dossier:d-1",
		memory.Entry{Content: mustJSON(t, observation.Content{Text: "x"})})

	assert.Equal(t, http.StatusForbidden, code,
		"a read-only slot makes the pool readable, never writable")
	assert.Contains(t, body, "dossier:d-1")
}

// A refusal, never a silent downgrade. Writing to the session scope instead
// would put the data somewhere plausible and hide the broken grant.
func TestAnUngrantedPoolIsRefusedRatherThanDowngradedToTheSession(t *testing.T) {
	h := newHandlerWithPools(t, stubPools{writeGrants: nil})

	postEntryRaw(t, h, poolWriteNS, poolWriteSess, observation.KindName, "?pool=ledger:l-9",
		memory.Entry{Content: mustJSON(t, observation.Content{Text: "x"})})

	// Nothing was written anywhere — not to the pool, and not to the session.
	assert.Empty(t, listScope(t, h, poolWriteNS, poolWriteSess, observation.KindName),
		"a refused pool write must not fall back to the session scope")
}

// Fail closed: a write-pool set that could not be READ is unknown, not empty,
// so the write is refused rather than falling through to the session scope.
// The stub would have granted the pool had discovery succeeded, so an
// implementation that widened access on failure is caught by the same fixture.
func TestAPoolWriteIsRefusedWhenTheGrantSetCannotBeRead(t *testing.T) {
	h := newHandlerWithPools(t, stubPools{
		writeGrants: []string{"dossier:d-1"},
		err:         errors.New("spicedb: read relationships: deadline exceeded"),
	})

	code, body := postEntryRaw(t, h, poolWriteNS, poolWriteSess, observation.KindName, "?pool=dossier:d-1",
		memory.Entry{Content: mustJSON(t, observation.Content{Text: "x"})})

	assert.Equal(t, http.StatusForbidden, code,
		"a grant set that could not be read is unknown, not empty")
	assert.Contains(t, body, "deadline exceeded",
		"the refusal must say WHY discovery failed, or an operator cannot tell a fault from a missing grant")
	assert.Empty(t, listScope(t, h, poolWriteNS, poolWriteSess, observation.KindName),
		"a discovery failure must not downgrade the write to the session scope either")
}

// No pools reader configured is also a refusal, not a downgrade: the server
// cannot prove the grant, so it cannot honour the destination.
func TestAPoolWriteIsRefusedWhenPoolsAreNotWiredAtAll(t *testing.T) {
	h := &handler{mem: memory.NewLocal(meminmem.NewBackend()), reg: tokens.NewRegistry()}
	h.reg.Set(memory.NamespacedName{Namespace: poolWriteNS, Name: poolWriteSess}, poolWriteTok, "")

	code, _ := postEntryRaw(t, h, poolWriteNS, poolWriteSess, observation.KindName, "?pool=dossier:d-1",
		memory.Entry{Content: mustJSON(t, observation.Content{Text: "x"})})

	assert.Equal(t, http.StatusForbidden, code)
	assert.Empty(t, listScope(t, h, poolWriteNS, poolWriteSess, observation.KindName),
		"an unprovable destination must not fall back to the session scope")
}

// No ?pool= is the pre-existing path and must be untouched.
func TestAWriteWithNoPoolStillGoesToTheSessionScope(t *testing.T) {
	h := newHandlerWithPools(t, stubPools{writeGrants: []string{"dossier:d-1"}})

	got := postEntry(t, h, poolWriteNS, poolWriteSess, observation.KindName, "",
		memory.Entry{Content: mustJSON(t, observation.Content{Text: "x"})})

	assert.Equal(t, memory.Scope{Kind: "session", ID: poolWriteNS + "/" + poolWriteSess}, got.Scope)
}

// The body stays untrusted for scope, exactly as before.
func TestABodySuppliedScopeIsStillIgnored(t *testing.T) {
	h := newHandlerWithPools(t, stubPools{writeGrants: []string{"dossier:d-1"}})

	got := postEntry(t, h, poolWriteNS, poolWriteSess, observation.KindName, "",
		memory.Entry{Scope: memory.Scope{Kind: "resource", ID: "dossier:d-1"},
			Content: mustJSON(t, observation.Content{Text: "x"})})

	assert.Equal(t, memory.Scope{Kind: "session", ID: poolWriteNS + "/" + poolWriteSess}, got.Scope,
		"naming a scope in the body must not reach it, with or without a grant")
}

// The request line WINS over the body, even when both name a pool the session
// may write. This is the sharper half of the untrusted-body property: the grant
// is not what makes the body trustworthy, the request line is — so an
// implementation that consulted the body "only when it is permitted anyway"
// still has to lose here.
func TestTheRequestLineWinsOverABodyNamingADifferentGrantedPool(t *testing.T) {
	h := newHandlerWithPools(t, stubPools{writeGrants: []string{"dossier:d-1", "ledger:l-9"}})

	got := postEntry(t, h, poolWriteNS, poolWriteSess, observation.KindName, "?pool=dossier:d-1",
		memory.Entry{Scope: memory.Scope{Kind: "resource", ID: "ledger:l-9"},
			Content: mustJSON(t, observation.Content{Text: "x"})})

	assert.Equal(t, memory.Scope{Kind: "resource", ID: "dossier:d-1"}, got.Scope,
		"the destination is the one on the request line; the body reaches nothing even when it names a granted pool")
}

// A GET naming a write pool lists the POOL, not the session. This is the read
// half of the same gate — the destination is decided for the whole keyed route,
// not only for POST — and the session's own entry is seeded so the assertion
// distinguishes "listed the pool" from "listed an empty session".
func TestAGetNamingAWritePoolListsThePoolNotTheSession(t *testing.T) {
	h := newHandlerWithPools(t, stubPools{writeGrants: []string{"dossier:d-1"}})

	postEntry(t, h, poolWriteNS, poolWriteSess, observation.KindName, "",
		memory.Entry{Content: mustJSON(t, observation.Content{Text: "session-note"})})
	postEntry(t, h, poolWriteNS, poolWriteSess, observation.KindName, "?pool=dossier:d-1",
		memory.Entry{Content: mustJSON(t, observation.Content{Text: "pool-note"})})

	got := listEntries(t, h, poolWriteNS, poolWriteSess, observation.KindName, "?pool=dossier:d-1")

	assert.Equal(t, []string{"pool-note"}, texts(t, got),
		"a GET naming a pool must list the pool's entries, not the session's")
	assert.Equal(t, []string{"session-note"}, texts(t, listScope(t, h, poolWriteNS, poolWriteSess, observation.KindName)),
		"and the session's own listing must be unaffected by the pool existing")
}

// A GET is held to the SAME proof: a read-only slot makes the pool readable
// through _search's own scope expansion, and this route is not that route.
// Refused rather than quietly answered from the session scope — a caller that
// addressed a pool and silently got the session's data back has no way to tell.
func TestAGetNamingAPoolWithoutAWriteGrantIsRefused(t *testing.T) {
	h := newHandlerWithPools(t, stubPools{readGrants: []string{"dossier:d-1"}})

	postEntry(t, h, poolWriteNS, poolWriteSess, observation.KindName, "",
		memory.Entry{Content: mustJSON(t, observation.Content{Text: "session-note"})})

	code, body := getEntriesRaw(t, h, poolWriteNS, poolWriteSess, observation.KindName, "?pool=dossier:d-1")

	assert.Equal(t, http.StatusForbidden, code,
		"a GET naming an unproved pool must be refused, never answered from the session scope")
	assert.NotContains(t, body, "session-note",
		"the refusal must not carry the session's own entries as if they were the pool's")
}

// A reserved route forces its own scope from the URL and cannot honour a
// ?pool= at all, so naming one there is refused rather than ignored: a caller
// that addressed a pool and silently got the session's own data back has no
// way to tell it asked the wrong question.
func TestAPoolNamedOnARouteThatCannotHonourItIsRefused(t *testing.T) {
	h := newHandlerWithPools(t, stubPools{writeGrants: []string{"dossier:d-1"}})

	for _, route := range []string{"_query", "_search", "_signal"} {
		req := httptest.NewRequest(http.MethodPost,
			"/memory/"+route+"/"+poolWriteNS+"/"+poolWriteSess+"?pool=dossier:d-1", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+poolWriteTok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"%s cannot dispatch to a pool, so ?pool= must be refused there, not ignored", route)
	}
}

// A malformed pool ref is refused by memory.ResourceScope's own validation.
func TestAMalformedPoolRefIsRefused(t *testing.T) {
	h := newHandlerWithPools(t, stubPools{writeGrants: []string{"dossier:d-1"}})
	for _, bad := range []string{"dossier", "dossier:a:b", "dossier:a/b", ":d-1", "dossier:"} {
		code, _ := postEntryRaw(t, h, poolWriteNS, poolWriteSess, observation.KindName, "?pool="+url.QueryEscape(bad),
			memory.Entry{Content: mustJSON(t, observation.Content{Text: "x"})})
		assert.Equal(t, http.StatusBadRequest, code, "pool ref %q", bad)
	}
}

// A query string Go's parser REJECTS must be a refusal, never an absent
// parameter.
//
// (*url.URL).Query() discards url.ParseQuery's error and omits the pairs that
// failed, so a bad percent-escape or a ';' in the pool pair makes Get("pool")
// return "" — which reads as "no pool was named" and writes to the SESSION
// scope with a 201, reporting success for a write that went somewhere the
// caller never addressed. That is exactly the silent downgrade this gate
// exists to prevent, arriving through how the parameter is READ rather than
// how it is proved.
//
// httpclient.PutToPool escapes its ref, so nothing in this repo can trigger it
// today; curl and any non-Go client can, and on the append-only path (where
// the facade's per-scope WriteMemory check is skipped in favour of provenance
// verification, which takes no scope) nothing downstream would notice.
//
// The raw query is sent VERBATIM — escaping it here would turn each case into
// a well-formed query carrying a weird VALUE, which is a different test
// (TestAMalformedPoolRefIsRefused above).
func TestAMalformedQueryStringIsRefusedRatherThanReadAsNoPool(t *testing.T) {
	cases := []struct {
		name     string
		rawQuery string
	}{
		{"invalid percent-escape in the pool value", "?pool=%zz"},
		{"semicolon in a granted pool pair", "?pool=dossier:d-1;x=1"},
		{"semicolon in an ungranted pool pair", "?pool=ledger:l-9;x=1"},
		{"an empty ?pool= names no destination", "?pool="},
	}
	for _, tc := range cases {
		t.Run(tc.name+": 400, and nothing written to the session scope", func(t *testing.T) {
			h := newHandlerWithPools(t, stubPools{writeGrants: []string{"dossier:d-1"}})

			code, _ := postEntryRaw(t, h, poolWriteNS, poolWriteSess, observation.KindName, tc.rawQuery,
				memory.Entry{Content: mustJSON(t, observation.Content{Text: "x"})})

			assert.Equal(t, http.StatusBadRequest, code)
			assert.Empty(t, listScope(t, h, poolWriteNS, poolWriteSess, observation.KindName),
				"a query the server could not read must never be treated as a write to the session scope")
		})
	}
}

// The same unreadable query on a reserved route is refused too: the guard that
// makes ?pool= loud there reads the parameter the same way, so it inherits the
// same hole if the query is not parsed once and checked.
func TestAMalformedQueryStringIsRefusedOnAReservedRouteToo(t *testing.T) {
	h := newHandlerWithPools(t, stubPools{writeGrants: []string{"dossier:d-1"}})

	req := httptest.NewRequest(http.MethodPost,
		"/memory/_query/"+poolWriteNS+"/"+poolWriteSess+"?pool=%zz", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+poolWriteTok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"?pool= on a route that cannot honour it must be refused even when the query does not parse")
}
