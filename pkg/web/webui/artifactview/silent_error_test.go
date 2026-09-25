package artifactview_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShell_CheckViewError_LogsUnderlyingError is the regression guard for a
// silent-error bug: when CheckView (the SpiceDB view check) returns an ERROR
// rather than a denial, the handler returns a generic 500 "authorization
// error" to the browser. If it ALSO drops the underlying error, an operator
// can never see the real diagnosis (e.g. "object definition `artifact` not
// found" when the live schema is missing the `artifact` type — the exact bug
// that motivated this test). The handler MUST log the real error with context.
func TestShell_CheckViewError_LogsUnderlyingError(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	av := fakeAV()
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) {
		return false, assert.AnError
	}
	av.logger = capLogger
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"a CheckView error must still surface as 500 to the browser")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, logged, "CheckView error must be logged, not silently dropped")
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "CheckView errored",
		"log must identify the failing check")
	assert.Contains(t, joined, "artifact-x",
		"log must carry the artifactID so an operator can locate the failure")
	assert.Contains(t, joined, assert.AnError.Error(),
		"log must include the underlying error — that is the whole point")
}
