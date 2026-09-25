package admind_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// stubDetailProjector scripts a DetailProjector: it returns detail for a
// specific (ns,name), (nil,nil) for "missing", and a scripted error otherwise.
type stubDetailProjector struct {
	resource string
	detail   *config.ResourceDetail
	err      error
}

func (s stubDetailProjector) Resource() string { return s.resource }
func (s stubDetailProjector) Detail(_ context.Context, _ client.Client, ns, name string) (*config.ResourceDetail, error) {
	if s.err != nil {
		return nil, s.err
	}
	if name == "missing" {
		return nil, nil // NotFound → handler 404
	}
	d := *s.detail
	d.Name = name
	d.Namespace = ns
	return &d, nil
}

func TestAdmindConfigDetail(t *testing.T) {
	// Snapshot + restore rather than a bare ResetDetail cleanup: the real
	// DetailProjectors (agents, tools, skills, sources, channels, identities,
	// users, providers, directory) are registered once via each owning
	// package's init() and never again, so a cleanup that only clears the
	// registry leaves every OTHER test in this binary — anything run after
	// this one, e.g. a test that drives the real "users" projector — unable
	// to find them for the rest of the process.
	original := config.AllDetail()
	config.ResetDetail()
	t.Cleanup(func() {
		config.ResetDetail()
		for _, p := range original {
			config.RegisterDetail(p)
		}
	})
	config.RegisterDetail(stubDetailProjector{resource: "widgets", detail: &config.ResourceDetail{
		Scope: "namespaced", Status: "Valid",
		Sections: []config.Section{
			{ID: "overview", Label: "Overview", Kind: config.SectionFields,
				Fields: []config.Field{{Label: "Kind", Value: "demo"}}},
		},
	}})
	config.RegisterDetail(stubDetailProjector{resource: "boom", err: errors.New("get exploded")})

	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	h := a.Handler()

	// view_config gates it: a non-admin subject is forbidden.
	w := do(t, h, http.MethodGet, "/admin/v1/config/widgets/ns/w1", "test-token", "user:bm9wZQ", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "detail needs view_config")

	// Admin, namespaced id "ns/w1" → 200; ns+name parsed from {id...}.
	w = do(t, h, http.MethodGet, "/admin/v1/config/widgets/ns/w1", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var d config.ResourceDetail
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &d))
	assert.Equal(t, "w1", d.Name)
	assert.Equal(t, "ns", d.Namespace)
	require.Len(t, d.Sections, 1)
	assert.Equal(t, "overview", d.Sections[0].ID)

	// Cluster-scoped id (single segment) → ns "" parsed.
	w = do(t, h, http.MethodGet, "/admin/v1/config/widgets/w1", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var cluster config.ResourceDetail
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &cluster))
	assert.Equal(t, "w1", cluster.Name)
	assert.Equal(t, "", cluster.Namespace)

	// Unknown resource → 404.
	w = do(t, h, http.MethodGet, "/admin/v1/config/unknown/x", "test-token", "user:YWRtaW4", "")
	assert.Equal(t, http.StatusNotFound, w.Code, "unregistered resource → 404")

	// Projector reports the CR does not exist (nil detail) → 404.
	w = do(t, h, http.MethodGet, "/admin/v1/config/widgets/ns/missing", "test-token", "user:YWRtaW4", "")
	assert.Equal(t, http.StatusNotFound, w.Code, "missing CR → 404")

	// Projector error → 500 (and logged).
	w = do(t, h, http.MethodGet, "/admin/v1/config/boom/ns/x", "test-token", "user:YWRtaW4", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code, "projector Detail error → 500")

	// The single-segment LIST route is unaffected by the detail wildcard: an
	// unregistered LIST projector for "widgets" still 404s (only detail is
	// registered here), proving the two routes dispatch independently.
	w = do(t, h, http.MethodGet, "/admin/v1/config/widgets", "test-token", "user:YWRtaW4", "")
	assert.Equal(t, http.StatusNotFound, w.Code, "list route dispatches to the list registry, not detail")
}
