package memory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// seedEntries writes n label entries straight into the backend, bypassing the
// facade's write door: this file is about the READ side.
func seedEntries(t *testing.T, b memory.Backend, scope memory.Scope, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		require.NoError(t, b.Put(context.Background(), memory.Entry{
			Scope:     scope,
			Kind:      "label",
			ID:        fmt.Sprintf("label-%03d", i),
			CreatedAt: time.Unix(0, int64(i)).UTC(),
			Content:   json.RawMessage(`{}`),
		}), "seed entry %d", i)
	}
}

// TestQueryTruncation is the defect this file exists for: a listing that
// stopped at --limit used to be indistinguishable from a complete one, so a
// reader could act on part of a result set believing it was all of it.
//
// Local.Query must answer the question the caller cannot answer for itself:
// did the limit end this listing, or did the data?
func TestQueryTruncation(t *testing.T) {
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}
	cases := []struct {
		name      string
		stored    int
		limit     int
		wantLen   int
		wantTrunc bool
	}{
		{name: "more stored than the limit: Truncated=true, entries capped at the limit", stored: 5, limit: 3, wantLen: 3, wantTrunc: true},
		{name: "exactly at the limit: complete answer, Truncated=false", stored: 3, limit: 3, wantLen: 3, wantTrunc: false},
		{name: "fewer than the limit: complete answer, Truncated=false", stored: 2, limit: 3, wantLen: 2, wantTrunc: false},
		{name: "empty scope: complete answer, Truncated=false", stored: 0, limit: 3, wantLen: 0, wantTrunc: false},
		{name: "no limit: whole scope returned, Truncated=false", stored: 5, limit: 0, wantLen: 5, wantTrunc: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := inmem.NewBackend()
			seedEntries(t, b, scope, tc.stored)
			m := memory.NewLocal(b)

			ctx := memory.WithSystemApproval(context.Background(), "operator:test")
			res, err := m.Query(ctx, memory.Query{Scope: scope, Limit: tc.limit})
			require.NoError(t, err, "Query must succeed")

			assert.Len(t, res.Entries, tc.wantLen, "the probe row must never reach the caller")
			assert.Equal(t, tc.wantTrunc, res.Truncated, "truncation verdict")
		})
	}
}

// TestQueryTruncationReportedForJSONCallers pins the machine-readable half: a
// script reading --json output must see the same fact the table prints, and
// must see it on BOTH answers — an absent field would leave "complete" and
// "old server" indistinguishable, which is the bug one layer up.
func TestQueryTruncationReportedForJSONCallers(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  memory.QueryResult
		want string
	}{
		{name: "truncated result carries Truncated:true", res: memory.QueryResult{Truncated: true}, want: `"Truncated":true`},
		{name: "complete result carries Truncated:false, not an absent field", res: memory.QueryResult{}, want: `"Truncated":false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.res)
			require.NoError(t, err, "Marshal QueryResult")
			assert.Contains(t, string(b), tc.want)
		})
	}
}
