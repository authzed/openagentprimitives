package workshopmcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreDraft_PostsBearerAndBytes_ReturnsRefAndDigest(t *testing.T) {
	var gotAuth, gotMethod, gotPath, gotContentType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		var readErr error
		gotBody, readErr = io.ReadAll(r.Body)
		require.NoError(t, readErr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"artifactRef": "mem://ns/x/workshop-draft/deadbeef.oap",
			"digest":      "deadbeef",
			"handle":      "ar-builder-x-0a1b2c",
			"artifactId":  "artifact-0123456789abcdef",
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "the-operator-bearer")

	s := &Server{}
	stored, err := s.storeDraft(context.Background(), []byte("oap-bytes-here"))
	require.NoError(t, err)
	assert.Equal(t, "mem://ns/x/workshop-draft/deadbeef.oap", stored.ArtifactRef)
	assert.Equal(t, "deadbeef", stored.Digest)
	assert.Equal(t, "ar-builder-x-0a1b2c", stored.Handle)
	assert.Equal(t, "artifact-0123456789abcdef", stored.ArtifactID)

	assert.Equal(t, "Bearer the-operator-bearer", gotAuth, "bearer sent from the token env var")
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/workshop/draft", gotPath)
	assert.Equal(t, "application/octet-stream", gotContentType)
	assert.Equal(t, "oap-bytes-here", string(gotBody), "the raw .oap bytes ride as the body")
}

func TestStoreDraft_TrimsTrailingSlashOnOperatorURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"artifactRef": "r", "digest": "d"})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL+"/")
	t.Setenv("token", "tok")

	s := &Server{}
	_, err := s.storeDraft(context.Background(), []byte("x"))
	require.NoError(t, err)
	assert.Equal(t, "/workshop/draft", gotPath, "no double slash even when OPERATOR_MEMORY_URL has a trailing one")
}

// TestStoreDraft_NonOKStatus_ReturnsError is the refusing-direction case: the
// operator's own denial (tuple false, workshop not Ready, …) must surface as
// an error, never as a successful (ref, digest) pair.
func TestStoreDraft_NonOKStatus_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "session does not hold workshop:ws-1#build", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "tok")

	s := &Server{}
	stored, err := s.storeDraft(context.Background(), []byte("oap-bytes"))
	require.Error(t, err)
	assert.Empty(t, stored.ArtifactRef)
	assert.Empty(t, stored.Digest)
	assert.Contains(t, err.Error(), "403")
	assert.Contains(t, err.Error(), "workshop:ws-1#build")
}

func TestStoreDraft_MissingOperatorEnv_ReturnsError(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		token string
	}{
		{"missing OPERATOR_MEMORY_URL", "", "tok"},
		{"missing token", "http://127.0.0.1:1", ""},
		{"missing both", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPERATOR_MEMORY_URL", tc.url)
			t.Setenv("token", tc.token)
			s := &Server{}
			_, err := s.storeDraft(context.Background(), []byte("x"))
			require.Error(t, err)
		})
	}
}

func TestStoreDraft_MalformedResponseBody_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "tok")

	s := &Server{}
	_, err := s.storeDraft(context.Background(), []byte("x"))
	require.Error(t, err)
}
