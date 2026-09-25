// pkg/memory/httpsrv/gates_test.go — the memory data plane's HTTP-side gates.
//
// Each test here pins one fail-closed decision ServeHTTP makes before it
// touches the facade: which requests may steer the shadow read source, which
// bearers may delete an entry, and whether a failed write leaves a
// server-side trace an operator can grep for.
package httpsrv_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // register turn/note/... Kinds
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// mutableKind is a registered Kind whose entries may be deleted per-entry —
// the only class the _entry route can reach at all, since Local.Delete
// refuses append-only Kinds unconditionally.
const (
	mutableKind   = "label"
	mutableEntry  = "label-1"
	mutableScopeN = "ns/n"
)

// labelEntry builds an Entry of the mutable Kind with the ID prefix that Kind
// requires.
func labelEntry(t *testing.T, id, text string) memory.Entry {
	t.Helper()
	content, err := json.Marshal(map[string]string{"label": text})
	require.NoError(t, err, "marshal label content")
	return memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: mutableScopeN},
		Kind:      mutableKind,
		ID:        id,
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   content,
	}
}

// ---------------------------------------------------------------------------
// F1 — ?backend= may steer only read routes, and only to a known value.
// ---------------------------------------------------------------------------

func TestBackendOverride_Gated(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{
			name:       "POST entry with ?backend=primary: refused 400 (steers the append-only pre-check)",
			method:     http.MethodPost,
			path:       "/memory/turn/ns/n?backend=primary",
			body:       `{"id":"turn-0-user","content":{"text":"x"}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "DELETE _entry with ?backend=primary: refused 400",
			method:     http.MethodDelete,
			path:       "/memory/_entry/ns/n?kind=label&id=label-1&backend=primary",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "POST _signal with ?backend=secondary: refused 400",
			method:     http.MethodPost,
			path:       "/memory/_signal/ns/n?backend=secondary",
			body:       `{"kind":"x"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "GET with unknown ?backend= value: refused 400, never a silent downgrade to primary",
			method:     http.MethodGet,
			path:       "/memory/turn/ns/n?backend=primry",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "GET with ?backend=primary: allowed 200",
			method:     http.MethodGet,
			path:       "/memory/turn/ns/n?backend=primary",
			wantStatus: http.StatusOK,
		},
		{
			name:       "GET with ?backend=secondary: allowed 200",
			method:     http.MethodGet,
			path:       "/memory/turn/ns/n?backend=secondary",
			wantStatus: http.StatusOK,
		},
		{
			name:       "POST _query with ?backend=secondary: allowed 200 (POST-bodied but read-only)",
			method:     http.MethodPost,
			path:       "/memory/_query/ns/n?backend=secondary",
			body:       `{}`,
			wantStatus: http.StatusOK,
		},
		{
			// `oap memory reindex --backend=` is a shipped caller, and _reindex
			// needs only ReadMemory — so the gate must be "the route's
			// capability is a read", NOT memoryRouteAccess (which is
			// writeAccess for _reindex because webd may not trigger one).
			name:       "POST _reindex with ?backend=secondary: allowed 200 (reads the backend, writes only indexes)",
			method:     http.MethodPost,
			path:       "/memory/_reindex/ns/n?backend=secondary",
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			reg := tokens.NewRegistry()
			srv := httptest.NewServer(httpsrv.NewHandler(mem, reg,
				httpsrv.WithDeleter(mem), httpsrv.WithReindexer(mem)))
			t.Cleanup(srv.Close)
			reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-1", "")

			var body *bytes.Reader
			if tc.body != "" {
				body = bytes.NewReader([]byte(tc.body))
			} else {
				body = bytes.NewReader(nil)
			}
			req, err := http.NewRequest(tc.method, srv.URL+tc.path, body)
			require.NoError(t, err, "NewRequest")
			req.Header.Set("Content-Type", "application/json")
			resp := do(t, authed(req, "tok-1"))
			defer resp.Body.Close()
			assert.Equal(t, tc.wantStatus, resp.StatusCode, "%s %s", tc.method, tc.path)
		})
	}
}

// ---------------------------------------------------------------------------
// F2 — no system token may delete an entry.
// ---------------------------------------------------------------------------

func TestDeleteEntry_SystemTokensRefused(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(reg *tokens.Registry)
		token      string
		wantStatus int
		wantGone   bool
	}{
		{
			name:       "channelsd system token: 403, entry survives",
			setup:      func(reg *tokens.Registry) { reg.SetChannelsdToken("chan-tok") },
			token:      "chan-tok",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "authzd system token: 403, entry survives",
			setup:      func(reg *tokens.Registry) { reg.SetAuthzdToken("authzd-tok") },
			token:      "authzd-tok",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "webd system token: 403, entry survives",
			setup:      func(reg *tokens.Registry) { reg.SetWebdToken("webd-tok") },
			token:      "webd-tok",
			wantStatus: http.StatusForbidden,
		},
		{
			name: "per-session token for its own session: 204, entry deleted",
			setup: func(reg *tokens.Registry) {
				reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "sess-tok", "")
			},
			token:      "sess-tok",
			wantStatus: http.StatusNoContent,
			wantGone:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			reg := tokens.NewRegistry()
			srv := httptest.NewServer(httpsrv.NewHandler(mem, reg, httpsrv.WithDeleter(mem)))
			t.Cleanup(srv.Close)
			tc.setup(reg)

			scope := memory.Scope{Kind: "session", ID: mutableScopeN}
			seedCtx := memory.WithSystemApproval(context.Background(), "system:test")
			_, err := mem.Put(seedCtx, labelEntry(t, mutableEntry, "seeded"))
			require.NoError(t, err, "seed mutable entry")

			req, err := http.NewRequest(http.MethodDelete,
				srv.URL+"/memory/_entry/ns/n?kind="+mutableKind+"&id="+mutableEntry, nil)
			require.NoError(t, err, "NewRequest DELETE")
			resp := do(t, authed(req, tc.token))
			defer resp.Body.Close()
			assert.Equal(t, tc.wantStatus, resp.StatusCode, "DELETE outcome")

			_, found, err := mem.Get(seedCtx, scope, mutableKind, mutableEntry)
			require.NoError(t, err, "Get after DELETE")
			assert.Equal(t, !tc.wantGone, found, "entry present after DELETE")
		})
	}
}

// ---------------------------------------------------------------------------
// F3/F4 — failures on the response and on the write path leave a log.
// ---------------------------------------------------------------------------

// recordingSink collects the messages logged through it so a test can assert
// that a failure left a server-side trace.
type recordingSink struct {
	mu   sync.Mutex
	msgs []string
}

func (s *recordingSink) Init(logr.RuntimeInfo)          {}
func (s *recordingSink) Enabled(int) bool               { return true }
func (s *recordingSink) WithValues(...any) logr.LogSink { return s }
func (s *recordingSink) WithName(string) logr.LogSink   { return s }

func (s *recordingSink) Info(_ int, msg string, _ ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
}

func (s *recordingSink) Error(_ error, msg string, _ ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
}

func (s *recordingSink) records() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

// failingWriter is a ResponseWriter whose body writes always fail, standing in
// for a client that hung up after the header went out.
type failingWriter struct {
	header http.Header
	code   int
}

func (w *failingWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *failingWriter) Write([]byte) (int, error) {
	return 0, assert.AnError
}

func (w *failingWriter) WriteHeader(code int) { w.code = code }

// serveWithLogger drives the handler directly (no httptest.Server) so the
// request context can carry a recording logger and the ResponseWriter can be
// one that fails.
func serveWithLogger(t *testing.T, h http.Handler, w http.ResponseWriter, req *http.Request) *recordingSink {
	t.Helper()
	sink := &recordingSink{}
	ctx := ctrllog.IntoContext(req.Context(), logr.New(sink))
	h.ServeHTTP(w, req.WithContext(ctx))
	return sink
}

func TestResponseEncodeFailure_IsLogged(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-1", "")
	h := httpsrv.NewHandler(mem, reg)

	req := httptest.NewRequest(http.MethodGet, "/memory/turn/ns/n", nil)
	req.Header.Set("Authorization", "Bearer tok-1")
	sink := serveWithLogger(t, h, &failingWriter{}, req)

	assert.NotEmpty(t, sink.records(),
		"a response body that failed to encode must leave a server-side log, not vanish")
}

func TestPutFailure_IsLogged(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-1", "")
	h := httpsrv.NewHandler(mem, reg)

	post := func(text string) (*httptest.ResponseRecorder, *recordingSink) {
		body, err := json.Marshal(turnEntry("turn-0-user", text))
		require.NoError(t, err, "Marshal")
		req := httptest.NewRequest(http.MethodPost, "/memory/turn/ns/n", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok-1")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		return rec, serveWithLogger(t, h, rec, req)
	}

	rec, _ := post("hello")
	require.Equal(t, http.StatusCreated, rec.Code, "first append lands")

	rec, sink := post("changed")
	require.Equal(t, http.StatusConflict, rec.Code, "conflicting re-put of append-only kind → 409")
	records := sink.records()
	require.NotEmpty(t, records, "a rejected write must leave a server-side log")
	joined := strings.Join(records, "|")
	assert.Contains(t, joined, "put entry",
		"the log must name the failing operation so an operator can grep for it")
}
