// pkg/memory/httpsrv/sentinel_roundtrip_test.go
//
// The two sides of the memory sentinel contract, end to end: a real
// httpsrv.NewHandler over an httptest.Server, driven by a real
// httpclient.Client. No hand-built http.Response anywhere — the point of this
// file is that the server's status/discriminator and the client's
// reconstruction AGREE, which a fabricated response cannot demonstrate.
package httpsrv_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/sentinel"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

const (
	rtNS    = "ns"
	rtName  = "n"
	rtToken = "tok-roundtrip"
)

var rtScope = memory.Scope{Kind: "session", ID: rtNS + "/" + rtName}

// errKG fails every KGQuerier mode with one fixed error, so the five KG client
// methods can be driven from a single fixture.
type errKG struct{ err error }

func (k *errKG) SearchFacts(context.Context, string, int) ([]memory.KGFact, error) {
	return nil, k.err
}
func (k *errKG) GetEntity(context.Context, string) (*memory.KGEntity, error) { return nil, k.err }
func (k *errKG) EntityFacts(context.Context, string) ([]memory.KGFact, error) {
	return nil, k.err
}
func (k *errKG) RelatedEntities(context.Context, string, int) ([]memory.KGEntity, error) {
	return nil, k.err
}
func (k *errKG) Communities(context.Context, string) ([]memory.KGCommunity, error) {
	return nil, k.err
}

// serveAllRoutes wires a handler whose Memory and KGQuerier both fail with err,
// and returns a real httpclient bound to it plus the recorded status of the
// last response the server sent.
func serveAllRoutes(t *testing.T, err error) (*httpclient.Client, *int) {
	t.Helper()
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: rtNS, Name: rtName}, rtToken, "")
	inner := httpsrv.NewHandler(
		&errMemory{Memory: memory.NewLocal(inmem.NewBackend()), err: err},
		reg,
		httpsrv.WithKG(&errKG{err: err}),
	)
	lastStatus := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: lastStatus}
		inner.ServeHTTP(rec, r)
	}))
	t.Cleanup(srv.Close)
	return httpclient.New(srv.URL, rtToken), lastStatus
}

// statusRecorder captures the status the handler chose so a case can assert the
// SERVER half of the contract in the same round trip as the client half.
type statusRecorder struct {
	http.ResponseWriter
	status *int
}

func (r *statusRecorder) WriteHeader(code int) {
	*r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// rtMethods is every httpclient call whose error the recall meta tools inspect:
// both Query routes, Search, and the five KG methods. Each returns only the
// error, which is the whole subject here.
var rtMethods = []struct {
	name string
	call func(ctx context.Context, c *httpclient.Client) error
}{
	{
		name: "Query/list route",
		call: func(ctx context.Context, c *httpclient.Client) error {
			_, err := c.Query(ctx, memory.Query{Scope: rtScope, Kinds: []string{"turn"}})
			return err
		},
	},
	{
		name: "Query/_query route",
		call: func(ctx context.Context, c *httpclient.Client) error {
			_, err := c.Query(ctx, memory.Query{
				Scope: rtScope, Kinds: []string{"turn"}, Tags: []string{"t"},
			})
			return err
		},
	},
	{
		name: "Search",
		call: func(ctx context.Context, c *httpclient.Client) error {
			_, err := c.Search(ctx, memory.SearchRequest{Scopes: []memory.Scope{rtScope}, Text: "x"})
			return err
		},
	},
	{
		name: "KGSearchFacts",
		call: func(ctx context.Context, c *httpclient.Client) error {
			_, err := c.KGSearchFacts(ctx, rtScope, "q", 10)
			return err
		},
	},
	{
		name: "KGGetEntity",
		call: func(ctx context.Context, c *httpclient.Client) error {
			_, err := c.KGGetEntity(ctx, rtScope, "6f1c1d2e-0000-4000-8000-000000000000")
			return err
		},
	},
	{
		name: "KGEntityFacts",
		call: func(ctx context.Context, c *httpclient.Client) error {
			_, err := c.KGEntityFacts(ctx, rtScope, "6f1c1d2e-0000-4000-8000-000000000000")
			return err
		},
	},
	{
		name: "KGRelatedEntities",
		call: func(ctx context.Context, c *httpclient.Client) error {
			_, err := c.KGRelatedEntities(ctx, rtScope, "6f1c1d2e-0000-4000-8000-000000000000", 10)
			return err
		},
	},
	{
		name: "KGCommunities",
		call: func(ctx context.Context, c *httpclient.Client) error {
			_, err := c.KGCommunities(ctx, rtScope)
			return err
		},
	},
}

// detailFor gives each sentinel a distinctive platform-authored detail clause,
// so a case can prove the server's message survived reconstruction instead of
// being replaced by the bare sentinel. The shapes mirror the real
// constructions: fieldpath.go's "did you mean", approval.go's perm+resource,
// graphiti's echoed entity id.
func detailFor(s error) string {
	switch {
	case errors.Is(s, memory.ErrInvalidQuery):
		return `FieldEquals path "outcom" is not a content key; did you mean "outcome"?`
	case errors.Is(s, memory.ErrMissingApproval):
		return "perm=read_memory resource=ns/n"
	case errors.Is(s, memory.ErrKGScopeMismatch):
		return "graphiti kg: get entity: 6f1c1d2e"
	default:
		return "no search backend is configured for this deployment"
	}
}

// TestSentinelRoundTrip_ServerToClient is the regression test for "the sentinel
// contract stops at the wire".
//
// httpsrv already decides, per sentinel, whether a memory failure is permanent
// and which status says so. httpclient collapsed every non-2xx into an
// unexported status error and rebuilt exactly one sentinel (Search's 404), so
// errors.Is(err, memory.ErrInvalidQuery) matched in-process and never over
// HTTP. In the runner sess.Mem IS an httpclient, so meta.platformAuthored
// always answered false there: a content guard could withhold the platform's
// own "did you mean" hint, and the model repeated the same bad query.
//
// One matrix, both halves: the status the server chose must equal the table's,
// and the error the client returns must satisfy errors.Is for the SAME
// sentinel while still carrying the server's message.
func TestSentinelRoundTrip_ServerToClient(t *testing.T) {
	ctx := context.Background()
	for _, row := range sentinel.Table {
		detail := detailFor(row.Err)
		injected := fmt.Errorf("%w: %s", row.Err, detail)
		for _, m := range rtMethods {
			t.Run(fmt.Sprintf("%s/%s: status=%d and errors.Is matches the sentinel", m.name, row.Code, row.Status),
				func(t *testing.T) {
					c, lastStatus := serveAllRoutes(t, injected)

					err := m.call(ctx, c)
					require.Error(t, err, "precondition: the injected failure surfaced as an error")
					assert.Equal(t, row.Status, *lastStatus,
						"the server must answer this sentinel with the status the shared table names")
					assert.ErrorIs(t, err, row.Err,
						"the client must rebuild the sentinel, or errors.Is is dead on the runner's memory path")
					assert.ErrorContains(t, err, detail,
						"the server's platform-authored detail is the payload; rebuilding must wrap it, not replace it")
				})
		}
	}
}

// TestSentinelRoundTrip_NonSentinelForbidden_NotLaundered guards the direction
// that matters more than the reconstruction itself: 403 is answered by TWO
// sentinels and by refusals that are no sentinel at all — handleKG's approval
// door, the read-only-token gate, an intermediary proxy. A status code alone
// cannot tell them apart, so a client that inferred a sentinel from the status
// would hand the model a forged authorization verdict, marked Trusted.
//
// The KG door refuses before dispatch and stamps no discriminator, so the
// client must fail toward an opaque transport error.
func TestSentinelRoundTrip_NonSentinelForbidden_NotLaundered(t *testing.T) {
	// A token scoped to another session: ServeHTTP refuses with a bare 403
	// before any sentinel is in play.
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: "other", Name: "sess"}, "tok-other", "")
	srv := httptest.NewServer(httpsrv.NewHandler(
		memory.NewLocal(inmem.NewBackend()), reg, httpsrv.WithKG(&errKG{err: errors.New("unreached")})))
	t.Cleanup(srv.Close)

	c := httpclient.New(srv.URL, "tok-other")
	_, err := c.Query(context.Background(), memory.Query{Scope: rtScope, Kinds: []string{"turn"}})

	require.Error(t, err, "precondition: a cross-session token is refused")
	assert.NotErrorIs(t, err, memory.ErrMissingApproval,
		"a 403 with no discriminator is not a capability refusal and must not be laundered into one")
	assert.NotErrorIs(t, err, memory.ErrKGScopeMismatch,
		"nor into the other sentinel that shares 403")
}
