package secretout

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHTTPPublisher_Publish_NonTwoXXIncludesFullResponseBody verifies that a
// non-2xx response's body (the operator's structural error message, e.g. the
// secretoutsrv 409 write-once message) is fully surfaced in the returned
// error — including messages long enough to exceed a fixed-size single Read
// call — so the agent sees the actual reason (e.g. "write-once") rather than
// a truncated or empty snippet. The value itself is never part of this body
// (secretoutsrv never echoes it), so there is no leak risk in surfacing it.
func TestHTTPPublisher_Publish_NonTwoXXIncludesFullResponseBody(t *testing.T) {
	// Longer than a small fixed read buffer, to catch a single short Read()
	// silently truncating the message.
	longPrefix := strings.Repeat("padding ", 40) // ~320 bytes
	body := longPrefix + `secret-output "kubeconfig" already satisfied for this session (write-once)`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, body, http.StatusConflict)
	}))
	t.Cleanup(srv.Close)

	p := NewHTTPPublisher(srv.URL, "ns", "sess1", "tok-own")
	err := p.Publish(context.Background(), "kubeconfig", []byte("the-secret-value"), "so-1")
	require.Error(t, err, "non-2xx must produce an error")
	assert.Contains(t, err.Error(), `already satisfied for this session (write-once)`,
		"the full operator message must survive into the returned error, not be truncated")
	assert.NotContains(t, err.Error(), "the-secret-value", "the value must never appear in the error")
}

// TestHTTPPublisher_Publish_Success204NoError verifies the happy path
// produces no error for a 204 response (secretoutsrv's success status).
func TestHTTPPublisher_Publish_Success204NoError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	p := NewHTTPPublisher(srv.URL, "ns", "sess1", "tok-own")
	err := p.Publish(context.Background(), "kubeconfig", []byte("v"), "so-1")
	assert.NoError(t, err)
}
