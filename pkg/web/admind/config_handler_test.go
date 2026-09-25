package admind_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

type stubProjector struct {
	resource string
	rows     []config.ResourceRow
	err      error
}

func (s stubProjector) Resource() string { return s.resource }
func (s stubProjector) List(_ context.Context, _ client.Client) ([]config.ResourceRow, error) {
	return s.rows, s.err
}

func TestAdmindConfigResource(t *testing.T) {
	// Isolate the package-global projector registry for this test binary.
	config.Reset()
	t.Cleanup(config.Reset)
	config.Register(stubProjector{resource: "widgets", rows: []config.ResourceRow{
		{Name: "w1", Scope: "cluster", Status: "Valid",
			Badges: []config.Badge{{Key: "kind", Value: "demo"}}, ManageCmd: "oap widget edit w1"},
		{Name: "w2", Namespace: "ns", Scope: "namespaced", Status: "Degraded"},
	}})
	config.Register(stubProjector{resource: "boom", err: errors.New("list exploded")})
	// rows left nil: mirrors a projector built with `var rows []ResourceRow`
	// that never appends when its backing CRD list is empty.
	config.Register(stubProjector{resource: "empty"})

	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	h := a.Handler()

	// view_config gates it: a non-admin subject is forbidden.
	w := do(t, h, http.MethodGet, "/admin/v1/config/widgets", "test-token", "user:bm9wZQ", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "config needs view_config")

	// Admin subject, known resource → 200 with the projected rows.
	w = do(t, h, http.MethodGet, "/admin/v1/config/widgets", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var rows []config.ResourceRow
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows))
	require.Len(t, rows, 2)
	assert.Equal(t, "w1", rows[0].Name)
	assert.Equal(t, "cluster", rows[0].Scope)
	assert.Equal(t, "oap widget edit w1", rows[0].ManageCmd)
	require.Len(t, rows[0].Badges, 1)
	assert.Equal(t, "kind", rows[0].Badges[0].Key)

	// Unknown resource → 404.
	w = do(t, h, http.MethodGet, "/admin/v1/config/unknown", "test-token", "user:YWRtaW4", "")
	assert.Equal(t, http.StatusNotFound, w.Code, "unregistered resource → 404")

	// Projector error → 500 (and logged).
	w = do(t, h, http.MethodGet, "/admin/v1/config/boom", "test-token", "user:YWRtaW4", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code, "projector List error → 500")

	// A projector returning a nil rows slice (empty CRD list, `var rows
	// []ResourceRow` never appended) must serialize as `[]`, not `null` — the
	// admin UI iterates the response directly and a bare `null` throws
	// client-side ("Symbol.iterator on null").
	w = do(t, h, http.MethodGet, "/admin/v1/config/empty", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "[]", strings.TrimSpace(w.Body.String()), "nil projector rows must serialize as [] not null")
	var emptyRows []config.ResourceRow
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &emptyRows))
	assert.NotNil(t, emptyRows)
	assert.Empty(t, emptyRows)
}
