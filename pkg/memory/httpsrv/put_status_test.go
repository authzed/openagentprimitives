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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// faultyBackend is a durable backend that is momentarily unavailable — a
// connection reset, a failover, a saturated pool. Exactly the class of fault
// httpclient.do is built to retry, and only ever if it is answered 5xx.
type faultyBackend struct {
	memory.Backend
	putErr error
	getErr error
}

func (b *faultyBackend) Put(ctx context.Context, e memory.Entry) error {
	if b.putErr != nil {
		return b.putErr
	}
	return b.Backend.Put(ctx, e)
}

func (b *faultyBackend) Get(ctx context.Context, scope memory.Scope, kind, id string) (memory.Entry, bool, error) {
	if b.getErr != nil {
		return memory.Entry{}, false, b.getErr
	}
	return b.Backend.Get(ctx, scope, kind, id)
}

// putErrMemory injects a specific error out of Put so the HTTP status mapping can
// be exercised per sentinel without having to arrange each condition for real.
type putErrMemory struct {
	memory.Memory
	err error
}

func (m *putErrMemory) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	if m.err != nil {
		return memory.Entry{}, m.err
	}
	return m.Memory.Put(ctx, e)
}

// postEntry POSTs e to srv as token and returns the status code.
func postEntry(t *testing.T, srv *httptest.Server, token string, e memory.Entry) int {
	t.Helper()
	body, err := json.Marshal(e)
	require.NoError(t, err, "marshal Entry")
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/"+e.Kind+"/ns/n", bytes.NewReader(body))
	require.NoError(t, err, "NewRequest POST")
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, authed(req, token))
	defer resp.Body.Close()
	return resp.StatusCode
}

// serveMemory wires a handler over mem with one session token.
func serveMemory(t *testing.T, mem memory.Memory) (*httptest.Server, string) {
	t.Helper()
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-put", "")
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg))
	t.Cleanup(srv.Close)
	return srv, "tok-put"
}

// TestPostEntry_TransientBackendFault_5xx_SoTheClientRetries is the defect:
// putStatus answered 400 for every error it did not recognize, including a
// durable-store fault. httpclient.do retries 5xx and treats 4xx as permanent, so
// a transient Postgres blip was reported to the caller as a bad request and the
// write was lost with no retry.
func TestPostEntry_TransientBackendFault_5xx_SoTheClientRetries(t *testing.T) {
	mem := memory.NewLocal(&faultyBackend{
		Backend: inmem.NewBackend(),
		putErr:  errors.New("write tcp 10.0.0.9:5432: connection reset by peer"),
	})
	srv, token := serveMemory(t, mem)

	got := postEntry(t, srv, token, turnEntry("turn-0-user", "hello"))
	assert.Equal(t, http.StatusInternalServerError, got,
		"a durable-backend fault must be 5xx so httpclient.do retries it, not 4xx-permanent")
}

// TestPostEntry_AppendOnlyPreCheckReadFault_5xx: Local.Put's append-only
// pre-check is a backend Get. Its failure is a store fault too — and the entry it
// guards is a tamper-evident audit record, so answering "bad request" and
// dropping the write is the worst available outcome.
func TestPostEntry_AppendOnlyPreCheckReadFault_5xx(t *testing.T) {
	mem := memory.NewLocal(&faultyBackend{
		Backend: inmem.NewBackend(),
		getErr:  errors.New("dial tcp 10.0.0.9:5432: i/o timeout"),
	})
	srv, token := serveMemory(t, mem)

	content, err := json.Marshal(map[string]string{"type": "turn_completed"})
	require.NoError(t, err, "marshal lifecycle content")
	got := postEntry(t, srv, token, memory.Entry{
		Kind: "lifecycle", ID: "lifecycle-evt-1", CreatedAt: time.Unix(0, 0).UTC(),
		Tags: []string{"event"}, Content: content,
	})
	assert.Equal(t, http.StatusInternalServerError, got,
		"a failed append-only pre-check read is a store fault (5xx), not a client error")
}

// TestPutStatusMapping covers every arm of the Put→status mapping in one place,
// so a future sentinel cannot be added without a deliberate decision about
// whether its caller should retry.
func TestPutStatusMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "unknown Kind / bad ID prefix (ErrInvalidEntry): 400, the caller must not retry",
			err:  fmt.Errorf("memory.Put: %w: Entry.ID %q has the wrong prefix", memory.ErrInvalidEntry, "nope-1"),
			want: http.StatusBadRequest,
		},
		{
			name: "append-only content conflict: 409",
			err:  fmt.Errorf("%w: ns/n lifecycle x", memory.ErrAppendOnlyConflict),
			want: http.StatusConflict,
		},
		{
			name: "unsigned append-only write: 403",
			err:  fmt.Errorf("%w: lifecycle", memory.ErrProvenanceRequired),
			want: http.StatusForbidden,
		},
		{
			name: "forged provenance: 403",
			err:  fmt.Errorf("%w: bad signature", memory.ErrBadProvenance),
			want: http.StatusForbidden,
		},
		{
			name: "missing capability approval: 403 permanent, never retried as transient",
			err:  fmt.Errorf("%w: perm=write_memory resource=ns/n", memory.ErrMissingApproval),
			want: http.StatusForbidden,
		},
		{
			name: "unrecognized error (a store or SpiceDB fault): 5xx so the client retries",
			err:  errors.New("relation \"memory_entries\" does not exist: SQLSTATE 42P01"),
			want: http.StatusInternalServerError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := &putErrMemory{Memory: memory.NewLocal(inmem.NewBackend()), err: tc.err}
			srv, token := serveMemory(t, mem)
			assert.Equal(t, tc.want, postEntry(t, srv, token, turnEntry("turn-0-user", "hello")))
		})
	}
}
