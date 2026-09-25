package identityd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	c := fake.NewClientBuilder().Build()
	return NewServer(Deps{
		K8s:             c,
		LinkSigner:      passthroughlink.New([]byte("test-key")),
		ExternalBaseURL: func() string { return "https://example.org" },
	})
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestNewServer_PanicsOnMissingDeps(t *testing.T) {
	require.Panics(t, func() { NewServer(Deps{}) })
	require.Panics(t, func() {
		NewServer(Deps{LinkSigner: passthroughlink.New([]byte("k")), ExternalBaseURL: func() string { return "https://e.org" }})
	})
}

func TestLinkStateStore_RoundTrip(t *testing.T) {
	s := newLinkStateStore()
	tok, err := s.NewState("link-raw-value", "b", "idp")
	require.NoError(t, err)
	assert.Len(t, tok, 32, "16 random bytes hex-encoded → 32 hex chars")

	raw, next, refusal := s.Consume(tok, "b", "idp")
	assert.Equal(t, stateAccepted, refusal)
	assert.Equal(t, "link-raw-value", raw)
	assert.Equal(t, "", next, "NewState binds no post-auth destination")

	// Single use: a second Consume of the same token must fail.
	_, _, refusal = s.Consume(tok, "b", "idp")
	assert.Equal(t, stateMissing, refusal)
}

func TestLinkStateStore_NewStateWithNext_RoundTrip(t *testing.T) {
	s := newLinkStateStore()
	tok, err := s.NewStateWithNext("link-raw-value", "https://trusted.example/artifact/v/abc", "b", "idp")
	require.NoError(t, err)

	raw, next, refusal := s.Consume(tok, "b", "idp")
	assert.Equal(t, stateAccepted, refusal)
	assert.Equal(t, "link-raw-value", raw)
	assert.Equal(t, "https://trusted.example/artifact/v/abc", next, "next must round-trip through Consume")
}

func TestLinkStateStore_Expired(t *testing.T) {
	s := newLinkStateStore()
	s.ttl = 0 // immediate expiry
	tok, err := s.NewState("anything", "b", "idp")
	require.NoError(t, err)
	_, _, refusal := s.Consume(tok, "b", "idp")
	assert.Equal(t, stateMissing, refusal, "expired entry must not be returned")
}
