package desktop_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
)

func TestLoopbackGuard(t *testing.T) {
	ok := desktop.LoopbackGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name       string
		host       string
		origin     string
		wantStatus int
	}{
		{name: "loopback IP host, no Origin: allowed", host: "127.0.0.1:8080", wantStatus: http.StatusOK},
		{name: "localhost host, no Origin: allowed", host: "localhost:8080", wantStatus: http.StatusOK},
		{name: "loopback host + loopback Origin: allowed", host: "127.0.0.1:8080", origin: "http://127.0.0.1:8080", wantStatus: http.StatusOK},
		{name: "foreign Host header: forbidden", host: "evil.example", wantStatus: http.StatusForbidden},
		{name: "loopback Host but foreign Origin: forbidden", host: "127.0.0.1:8080", origin: "https://evil.example", wantStatus: http.StatusForbidden},
		{name: "loopback Host, malformed Origin: forbidden", host: "127.0.0.1:8080", origin: "://not-a-url", wantStatus: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			ok.ServeHTTP(rec, req)
			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}
