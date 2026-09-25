package httpsrv_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // register turn/label/... Kinds
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// TestQueryTruncationCrossesTheWire pins that the truncation fact survives the
// HTTP hop. Every caller outside the operator — `oap memory list`, the runner's
// recall tools — reads memory through httpclient, so a fact the facade computes
// and the wire drops is a fact nobody sees.
//
// Both routes are covered because httpclient picks between them on the shape of
// the Query: a single-Kind scope listing takes GET, anything richer takes
// POST /_query. A fix landing on one of them is half a fix.
func TestQueryTruncationCrossesTheWire(t *testing.T) {
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}

	cases := []struct {
		name      string
		stored    int
		query     memory.Query
		wantLen   int
		wantTrunc bool
	}{
		{
			name:      "GET listing route, limit cut it short: Truncated=true",
			stored:    5,
			query:     memory.Query{Scope: scope, Kinds: []string{"label"}, Limit: 3},
			wantLen:   3,
			wantTrunc: true,
		},
		{
			name:      "GET listing route, exactly at the limit: Truncated=false",
			stored:    3,
			query:     memory.Query{Scope: scope, Kinds: []string{"label"}, Limit: 3},
			wantLen:   3,
			wantTrunc: false,
		},
		{
			name:      "POST _query route, limit cut it short: Truncated=true",
			stored:    5,
			query:     memory.Query{Scope: scope, Kinds: []string{"label"}, Tags: []string{"t"}, Limit: 3},
			wantLen:   3,
			wantTrunc: true,
		},
		{
			name:      "POST _query route, exactly at the limit: Truncated=false",
			stored:    3,
			query:     memory.Query{Scope: scope, Kinds: []string{"label"}, Tags: []string{"t"}, Limit: 3},
			wantLen:   3,
			wantTrunc: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := inmem.NewBackend()
			for i := 0; i < tc.stored; i++ {
				require.NoError(t, b.Put(context.Background(), memory.Entry{
					Scope:     scope,
					Kind:      "label",
					ID:        fmt.Sprintf("label-%03d", i),
					CreatedAt: time.Unix(0, int64(i)).UTC(),
					Tags:      []string{"t"},
					Content:   json.RawMessage(`{}`),
				}), "seed entry %d", i)
			}
			reg := tokens.NewRegistry()
			reg.Set(memory.NamespacedName{Namespace: "ns", Name: "sess"}, "tok-1", "")
			srv := httptest.NewServer(httpsrv.NewHandler(memory.NewLocal(b), reg))
			t.Cleanup(srv.Close)

			res, err := httpclient.New(srv.URL, "tok-1").Query(context.Background(), tc.query)
			require.NoError(t, err, "Query over the wire")
			assert.Len(t, res.Entries, tc.wantLen)
			assert.Equal(t, tc.wantTrunc, res.Truncated, "truncation verdict after the round trip")
		})
	}
}
