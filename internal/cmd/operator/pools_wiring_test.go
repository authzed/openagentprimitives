package main

// The resource-memory pool route spent its whole existence DEFINED AND
// UNREACHABLE — the exact shape pttag_minter_test.go documents for the
// pt-tag mint route. The resource scope type (pkg/memory/resourcescope.go),
// slot-grant pool discovery (pkg/memory/pools), the per-pool approval mint
// (httpsrv's mintPoolApprovals) and the search-scope expansion (httpsrv's
// handleSearch) were all merged and covered by their own packages' tests —
// and nothing anywhere called httpsrv.WithPools, so every _search request
// got the session scope alone, exactly as if resource pools did not exist.
//
// So the assertion worth having is not "pool discovery / scope expansion
// works" (pkg/memory/pools and pkg/memory/httpsrv's own tests already cover
// that) but "a handler built from newMemHandlerOpts — the SAME assembly
// run() calls, not a hand-picked option list this test invents — actually
// SPANS a resource's pool when the session holds a slot grant on it". That
// distinction matters: an earlier version of this test built its own
// httpsrv.NewHandler(mem, reg, httpsrv.WithPools(reader)) call directly, which
// proved the OPTION works but could not prove main.go's real wiring still
// calls it — commenting out the WithPools line inside what was then run()'s
// inline option list left every test in this package green. Going through
// newMemHandlerOpts (extracted so it can be called from here) closes that
// gap: deleting the WithPools line now reddens this test by name.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/pools"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// stubPoolsReader is a fixed pools.RelationshipReader. The filter argument is
// deliberately ignored — pools.ForSession narrows to slot_grant_* itself, the
// same division of labor pkg/memory/httpsrv's own poolFakeReader relies on.
type stubPoolsReader struct {
	rels []authz.Relation
}

func (s stubPoolsReader) ReadRelationships(context.Context, pools.RelFilter) ([]authz.Relation, error) {
	return s.rels, nil
}

// spySearchMemory is a memory.Memory stub built for this one question: what
// Scopes did ServeHTTP actually hand to Search? Put/Query/SendSignal should
// never be reached by a _search request, so each calls t.Fatal — the
// pttag_minter_test.go habit — meaning a future change that starts depending
// on them here says so out loud instead of silently passing.
type spySearchMemory struct {
	t         *testing.T
	gotScopes []memory.Scope
}

func (s *spySearchMemory) Put(context.Context, memory.Entry) (memory.Entry, error) {
	s.t.Helper()
	s.t.Fatal("Put called: this test asserts search scope expansion, not writes")
	return memory.Entry{}, nil
}

func (s *spySearchMemory) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	s.t.Helper()
	s.t.Fatal("Query called: this test exercises _search, not _query")
	return memory.QueryResult{}, nil
}

func (s *spySearchMemory) SendSignal(context.Context, memory.Signal) error {
	s.t.Helper()
	s.t.Fatal("SendSignal called: this test exercises _search, not signals")
	return nil
}

func (s *spySearchMemory) Search(_ context.Context, req memory.SearchRequest) (memory.MergedSearchResult, error) {
	s.gotScopes = req.Scopes
	return memory.MergedSearchResult{}, nil
}

const (
	poolsWiringNS   = "ns-a"
	poolsWiringSess = "sess-a"
	poolsWiringTok  = "tok-pools-wiring"
)

// newPoolsWiringHandler builds a handler from newMemHandlerOpts — the exact
// function run() calls to assemble the memory HTTP handler's options — with
// every dependency but PoolsReader left at its zero value. Those zero values
// are safe here: none of the routes they gate (_artifact, _pttag_mint,
// _pttag_resolve, _entry, _reindex, _kg) are the one this test dispatches
// to (_search), and newMemHandlerOpts's own construction calls (
// newPtTagSpiceDBAdapter, newPtTagResolver, newAttachmentExploder) none of
// them dereference a nil client at construction time — only at first use,
// which this test never triggers. MemLocal is the one exception: it is
// real, not nil, because a nil *memorypkg.Local assigned into the
// EntryDeleter/EntryReindexer interface fields would be a typed-nil
// interface landmine (AGENTS.md's "Nil interfaces" section) even though
// unreached here — cheap enough to just not create it.
//
// reader == nil means PoolsReader is never populated, i.e. this test's
// mutation target line, httpsrv.WithPools(d.PoolsReader) inside
// newMemHandlerOpts, is called with a true nil interface — indistinguishable
// from the option never having been added, and mintPoolApprovals's own nil
// check already treats it that way.
func newPoolsWiringHandler(t *testing.T, mem memory.Memory, reader pools.RelationshipReader) http.Handler {
	t.Helper()
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: poolsWiringNS, Name: poolsWiringSess}, poolsWiringTok, "")
	ml := memory.NewLocal(memoryinmem.NewBackend())
	opts := newMemHandlerOpts(memHandlerDeps{
		MemLocal:    ml,
		PoolsReader: reader,
	})
	return httpsrv.NewHandler(mem, reg, opts...)
}

// TestSearchSpansAResourcePoolWhenWiredTheWayTheOperatorWiresIt is the pin
// that would have caught this task's exact defect, and would catch a later
// change that stops calling WithPools INSIDE newMemHandlerOpts — the
// function run() actually calls, not a copy of its shape.
func TestSearchSpansAResourcePoolWhenWiredTheWayTheOperatorWiresIt(t *testing.T) {
	poolScope, err := memory.ResourceScope("widget", "gadget-1")
	require.NoError(t, err)

	rels := []authz.Relation{{
		ResourceType: "widget",
		ResourceID:   "gadget-1",
		Relation:     authz.SlotGrantRelationName("read_memory"),
		SubjectType:  "agentsession",
		SubjectID:    poolsWiringNS + "/" + poolsWiringSess,
	}}

	doSearch := func(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost,
			"/memory/_search/"+poolsWiringNS+"/"+poolsWiringSess, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+poolsWiringTok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("wired the way the operator wires it: search spans the resource pool", func(t *testing.T) {
		mem := &spySearchMemory{t: t}
		h := newPoolsWiringHandler(t, mem, stubPoolsReader{rels: rels})
		rec := doSearch(t, h)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, mem.gotScopes, poolScope,
			"a session holding a slot grant on the resource must have its pool included in what _search actually spans")
	})

	t.Run("not wired at all: search stays session-scope only", func(t *testing.T) {
		mem := &spySearchMemory{t: t}
		h := newPoolsWiringHandler(t, mem, nil)
		rec := doSearch(t, h)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, []memory.Scope{{Kind: "session", ID: poolsWiringNS + "/" + poolsWiringSess}}, mem.gotScopes,
			"without a pools reader the search must stay session-scope only, matching the tree before this option was called")
	})
}
