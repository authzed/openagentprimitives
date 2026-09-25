// pkg/memory/httpclient/putpool_test.go — PutToPool's request shape.
//
// The shape IS the contract: the server refuses to read a destination out of
// the body at all, so a pool write that put the pool anywhere but the request
// line would be silently written to the session scope instead. These tests
// pin the query parameter specifically, which the shared `captured` helper in
// httpclient_test.go does not record.
package httpclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// The URL names the SESSION (that is what the bearer authorizes) and the pool
// travels as ?pool=, never in the body.
func TestPutToPool_NamesTheSessionInThePathAndThePoolInTheQuery(t *testing.T) {
	poolScope, err := memory.ResourceScope("dossier", "d-1")
	require.NoError(t, err)

	var gotPath, gotPool, gotMethod string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotPool = r.Method, r.URL.Path, r.URL.Query().Get("pool")
		gotBody, _ = io.ReadAll(r.Body)
		// Echo what the SERVER decided the scope is — the pool — exactly as
		// httpsrv does after proving the grant.
		require.NoError(t, json.NewEncoder(w).Encode(memory.Entry{
			Scope: poolScope, Kind: "observation", ID: "obs-1",
		}))
	}))
	t.Cleanup(srv.Close)

	out, err := New(srv.URL, "tok").PutToPool(context.Background(),
		memory.Scope{Kind: "session", ID: "ns-a/sess-a"}, poolScope,
		memory.Entry{Kind: "observation", ID: "obs-1", Content: json.RawMessage(`{"text":"x"}`)})
	require.NoError(t, err, "PutToPool")

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/memory/observation/ns-a/sess-a", gotPath,
		"the path names the session the bearer authorizes, not the pool")
	assert.Equal(t, "dossier:d-1", gotPool,
		"the destination rides the request line, where the server can prove it")

	var sent memory.Entry
	require.NoError(t, json.Unmarshal(gotBody, &sent))
	assert.Empty(t, sent.Scope.ID,
		"the body carries no destination — the server would ignore one, and sending it invites a caller to believe otherwise")
	assert.Equal(t, poolScope, out.Scope, "the stored entry's scope is whatever the server decided")
}

// A session scope handed to PutToPool is a call-site bug, refused before a
// request is made rather than sent to be rejected as a malformed ref.
func TestPutToPool_RefusesANonResourceScopeWithoutCallingTheServer(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(srv.Close)

	_, err := New(srv.URL, "tok").PutToPool(context.Background(),
		memory.Scope{Kind: "session", ID: "ns-a/sess-a"},
		memory.Scope{Kind: "session", ID: "ns-a/other"},
		memory.Entry{Kind: "observation", ID: "obs-1"})

	require.Error(t, err, "a session scope is not a pool")
	assert.Contains(t, err.Error(), "resource scope")
	assert.False(t, called, "the refusal must be local: no request may be made")
}

// QueryPool is the READ half of the same addressing, and its shape is the
// contract for the same reason: the ordinary Query route builds its URL from
// scopePath, which assumes a "<ns>/<name>" session ID, so a resource scope
// produced "/memory/observation/dossier:d-1/" and the real server answered 404.
// That 404 is what failed the FIRST pool write over the production
// composition — provenance.SigningMemory seeds a pool's chain by reading it
// back — so the route existing at all is the fix, and its shape is what makes
// the server able to prove the grant.
func TestQueryPool_NamesTheSessionInThePathAndThePoolInTheQuery(t *testing.T) {
	poolScope, err := memory.ResourceScope("dossier", "d-1")
	require.NoError(t, err)

	var gotPath, gotPool, gotMethod, gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotPool, gotLimit = r.URL.Query().Get("pool"), r.URL.Query().Get("limit")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{
			Entries: []memory.Entry{{Scope: poolScope, Kind: "observation", ID: "obs-1"}},
		}))
	}))
	t.Cleanup(srv.Close)

	out, err := New(srv.URL, "tok").QueryPool(context.Background(),
		memory.Scope{Kind: "session", ID: "ns-a/sess-a"}, poolScope,
		memory.Query{Scope: poolScope, Kinds: []string{"observation"}, Limit: 5})
	require.NoError(t, err, "QueryPool")

	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, "/memory/observation/ns-a/sess-a", gotPath,
		"the path names the session the bearer authorizes, not the pool")
	assert.Equal(t, "dossier:d-1", gotPool,
		"the destination rides the request line, where httpsrv's destinationFor proves the grant — on every method, not only POST")
	assert.Equal(t, "5", gotLimit, "a limit must survive alongside the destination, not replace it")
	require.Len(t, out.Entries, 1)
	assert.Equal(t, poolScope, out.Entries[0].Scope)
}

// A rich query cannot be addressed to a pool: the destination is honoured only
// on the keyed route, and a rich query goes to _query, which would then serve
// the SESSION's scope. Answering a pool question with session data is the
// silent downgrade the whole destination gate exists to prevent, so the
// mismatch is refused locally and named.
func TestQueryPool_RefusesARichQueryRatherThanServingTheSessionScope(t *testing.T) {
	poolScope, err := memory.ResourceScope("dossier", "d-1")
	require.NoError(t, err)

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(srv.Close)

	for name, q := range map[string]memory.Query{
		"two kinds":  {Scope: poolScope, Kinds: []string{"observation", "transcript"}},
		"no kind":    {Scope: poolScope},
		"with tags":  {Scope: poolScope, Kinds: []string{"observation"}, Tags: []string{"process"}},
		"with an id": {Scope: poolScope, Kinds: []string{"observation"}, IDs: []string{"obs-1"}},
	} {
		t.Run(name+": refused locally, no request made", func(t *testing.T) {
			_, qerr := New(srv.URL, "tok").QueryPool(context.Background(),
				memory.Scope{Kind: "session", ID: "ns-a/sess-a"}, poolScope, q)
			require.Error(t, qerr)
			assert.Contains(t, qerr.Error(), "single-Kind listing")
		})
	}
	assert.False(t, called, "no request may be made for a query the route cannot express")
}

// A session scope handed to QueryPool is the same call-site bug PutToPool
// refuses, answered the same way.
func TestQueryPool_RefusesANonResourceScopeWithoutCallingTheServer(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(srv.Close)

	_, err := New(srv.URL, "tok").QueryPool(context.Background(),
		memory.Scope{Kind: "session", ID: "ns-a/sess-a"},
		memory.Scope{Kind: "session", ID: "ns-a/other"},
		memory.Query{Kinds: []string{"observation"}})

	require.Error(t, err, "a session scope is not a pool")
	assert.Contains(t, err.Error(), "resource scope")
	assert.False(t, called, "the refusal must be local: no request may be made")
}
