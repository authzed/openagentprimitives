package health_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/health"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

func TestHealth_ServesOKOnTrustedOrigin(t *testing.T) {
	s, err := webui.NewServer(nil, nil,
		func() string { return "trusted.example" }, func() string { return "sandbox.example" },
		nil, nil, []webui.WebUI{health.New()})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodGet, "http://trusted.example/healthz", nil)
	r.Host = "trusted.example"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
}

func TestHealth_RegistersViaInit(t *testing.T) {
	names := map[string]bool{}
	for _, u := range registry.All() {
		names[u.Name()] = true
	}
	assert.True(t, names["health"], "health UI must self-register via init()")
}
