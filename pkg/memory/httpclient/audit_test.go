package httpclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/stretchr/testify/require"
)

func TestQueryAuditNeverFallsBackToFilteredSessionQuery(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, "/audit/ns/session", r.URL.Path)
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "Bearer admin", r.Header.Get("Authorization"))
				w.WriteHeader(status)
			}))
			defer server.Close()
			_, err := httpclient.New(server.URL, "admin").QueryAudit(context.Background(), memory.Scope{Kind: "session", ID: "ns/session"})
			require.Error(t, err)
			require.Equal(t, 1, calls)
		})
	}
}

func TestQueryAuditRefusesNonSessionScopes(t *testing.T) {
	c := httpclient.New("http://invalid.invalid", "admin")
	for _, scope := range []memory.Scope{{Kind: "resource", ID: "ns/session"}, {Kind: "session", ID: "ns/session/other"}, {Kind: "session", ID: "ns/../session"}} {
		_, err := c.QueryAudit(context.Background(), scope)
		require.ErrorContains(t, err, "requires a session scope")
	}
}
