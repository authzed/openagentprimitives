package httpsrv_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// errMemory injects a specific error out of every non-Put operation, so the
// HTTP status mapping can be exercised per (route, sentinel) without having to
// arrange each condition for real. It also satisfies the optional
// EntryDeleter / EntryReindexer interfaces so the _entry and _reindex routes
// can be mounted.
type errMemory struct {
	memory.Memory
	err error
}

func (m *errMemory) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	if m.err != nil {
		return memory.QueryResult{}, m.err
	}
	return m.Memory.Query(ctx, q)
}

func (m *errMemory) Search(ctx context.Context, r memory.SearchRequest) (memory.MergedSearchResult, error) {
	if m.err != nil {
		return memory.MergedSearchResult{}, m.err
	}
	return m.Memory.Search(ctx, r)
}

func (m *errMemory) SendSignal(ctx context.Context, s memory.Signal) error {
	if m.err != nil {
		return m.err
	}
	return m.Memory.SendSignal(ctx, s)
}

func (m *errMemory) Reindex(context.Context, memory.Scope) (int, error) { return 0, m.err }

func (m *errMemory) Delete(context.Context, memory.Scope, string, string) error { return m.err }

// serveReadRoutes wires a handler over mem with every optional route mounted
// and one session token.
func serveReadRoutes(t *testing.T, mem *errMemory) (*httptest.Server, string) {
	t.Helper()
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-read", "")
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg,
		httpsrv.WithDeleter(mem), httpsrv.WithReindexer(mem)))
	t.Cleanup(srv.Close)
	return srv, "tok-read"
}

// call issues one request against srv and returns the status code.
func call(t *testing.T, srv *httptest.Server, token, method, path, body string) int {
	t.Helper()
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	var req *http.Request
	var err error
	if rdr != nil {
		req, err = http.NewRequest(method, srv.URL+path, rdr)
	} else {
		req, err = http.NewRequest(method, srv.URL+path, nil)
	}
	require.NoError(t, err, "NewRequest %s %s", method, path)
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, authed(req, token))
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestQueryRoute_UnresolvableFieldPath_400_NotRetried is the defect: every
// Memory.Query error became a 500, and httpclient.do retries 5xx eight times
// over a ~17s backoff budget. Local.Query hard-errors on a FieldEquals path
// that names no content key of the queried Kind — a caller bug no retry can
// fix — and the agent's query_memory tool builds Kinds and FieldEquals
// straight from LLM-supplied arguments, so a model guessing a field name burnt
// most of a turn before the "did you mean" ever reached it.
//
// Real Local, real registered Kind, real refusal: nothing is injected here.
func TestQueryRoute_UnresolvableFieldPath_400_NotRetried(t *testing.T) {
	mem := &errMemory{Memory: memory.NewLocal(inmem.NewBackend())}
	srv, token := serveReadRoutes(t, mem)

	body, err := json.Marshal(memory.Query{
		Kinds: []string{"channel_msg_ref"},
		// The Go field name, not the `json` tag it is stored under — the
		// exact slip validateFieldPaths exists to catch.
		FieldEquals: []memory.FieldFilter{{Path: "TurnIndex", Value: 3}},
	})
	require.NoError(t, err, "marshal Query")

	assert.Equal(t, http.StatusBadRequest,
		call(t, srv, token, http.MethodPost, "/memory/_query/ns/n", string(body)),
		"an unresolvable FieldEquals path is a caller bug: 400 permanent, never eight retries")
}

// TestReadRouteStatusMapping covers every route that was mapping its whole
// error space to 500. The putStatus 4xx/5xx split was added for exactly this
// reason and reached only Put; each row here is a route it never reached.
func TestReadRouteStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		err    error
		want   int
	}{
		{
			name:   "POST _query with no capability approval: 403 permanent, not a retried 500",
			method: http.MethodPost, path: "/memory/_query/ns/n", body: `{}`,
			err:  fmt.Errorf("%w: perm=read_memory resource=ns/n", memory.ErrMissingApproval),
			want: http.StatusForbidden,
		},
		{
			name:   "POST _query with a store fault: 500 so httpclient.do retries it",
			method: http.MethodPost, path: "/memory/_query/ns/n", body: `{}`,
			err:  errors.New("dial tcp 10.0.0.9:5432: i/o timeout"),
			want: http.StatusInternalServerError,
		},
		{
			name:   "GET {kind} with an unresolvable field path: 400, same refusal as _query",
			method: http.MethodGet, path: "/memory/turn/ns/n",
			err:  fmt.Errorf("memory: query: FieldEquals path %q is unresolvable: %w", "Nope", memory.ErrInvalidQuery),
			want: http.StatusBadRequest,
		},
		{
			name:   "GET {kind} with no capability approval: 403 permanent",
			method: http.MethodGet, path: "/memory/turn/ns/n",
			err:  fmt.Errorf("%w: perm=read_memory resource=ns/n", memory.ErrMissingApproval),
			want: http.StatusForbidden,
		},
		{
			name:   "POST _search with no capability approval: 403 permanent",
			method: http.MethodPost, path: "/memory/_search/ns/n", body: `{}`,
			err:  fmt.Errorf("%w: perm=read_memory resource=ns/n", memory.ErrMissingApproval),
			want: http.StatusForbidden,
		},
		{
			name:   "POST _search with no providers configured: 404, unchanged",
			method: http.MethodPost, path: "/memory/_search/ns/n", body: `{}`,
			err:  memory.ErrNoSearchProviders,
			want: http.StatusNotFound,
		},
		{
			name:   "POST _reindex with no capability approval: 403 permanent",
			method: http.MethodPost, path: "/memory/_reindex/ns/n",
			err:  fmt.Errorf("%w: perm=read_memory resource=ns/n", memory.ErrMissingApproval),
			want: http.StatusForbidden,
		},
		{
			name:   "POST _signal with no capability approval: 403 permanent",
			method: http.MethodPost, path: "/memory/_signal/ns/n", body: `{"kind":"lifecycle/turn.completed"}`,
			err:  fmt.Errorf("%w: perm=write_memory resource=ns/n", memory.ErrMissingApproval),
			want: http.StatusForbidden,
		},
		{
			name:   "DELETE _entry with no capability approval: 403 permanent",
			method: http.MethodDelete, path: "/memory/_entry/ns/n?kind=turn&id=turn-000000-user",
			err:  fmt.Errorf("%w: perm=delete_memory resource=ns/n", memory.ErrMissingApproval),
			want: http.StatusForbidden,
		},
		{
			name:   "POST _reindex with a store fault: 500 so the caller retries",
			method: http.MethodPost, path: "/memory/_reindex/ns/n",
			err:  errors.New("connection reset by peer"),
			want: http.StatusInternalServerError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := &errMemory{Memory: memory.NewLocal(inmem.NewBackend()), err: tc.err}
			srv, token := serveReadRoutes(t, mem)
			assert.Equal(t, tc.want, call(t, srv, token, tc.method, tc.path, tc.body))
		})
	}
}
