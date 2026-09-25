package httpsrv

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestRoutePermission(t *testing.T) {
	cases := []struct {
		route, method string
		want          memory.Permission
		ok            bool
	}{
		{"_query", http.MethodPost, memory.ReadMemory, true},
		{"_search", http.MethodPost, memory.ReadMemory, true},
		{"_kg", http.MethodGet, memory.ReadKG, true},
		{"_entry", http.MethodDelete, memory.DeleteMemory, true},
		{"_signal", http.MethodPost, memory.WriteMemory, true},
		{"_reindex", http.MethodPost, memory.ReadMemory, true},
		{"transcript", http.MethodGet, memory.ReadMemory, true},   // handleKind GET
		{"transcript", http.MethodPost, memory.WriteMemory, true}, // handleKind PUT
		{"_publisher_key", http.MethodPost, "", false},            // not a data route
	}
	for _, tc := range cases {
		got, ok := routePermission(tc.route, tc.method)
		assert.Equal(t, tc.ok, ok, "route=%s method=%s", tc.route, tc.method)
		if tc.ok {
			assert.Equal(t, tc.want, got, "route=%s method=%s", tc.route, tc.method)
		}
	}
}
