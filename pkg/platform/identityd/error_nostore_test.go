package identityd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestWriteErrorSetsNoStore pins the no-silent-errors fix: identityd error
// pages must carry Cache-Control: no-store so a transient 403 (e.g. before an
// IdP/authenticator is configured) is never cached by the browser or a shared
// LB and stuck across the user's retries. Exercises the nil-renderer fallback
// path (no webui renderer on the context), which still must set the header.
func TestWriteErrorSetsNoStore(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/link", nil)

	s.writeError(w, r, http.StatusForbidden, "Sign-in unavailable", "no provider configured")

	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, http.StatusForbidden, w.Code)
}
