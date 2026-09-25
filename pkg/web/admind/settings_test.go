package admind_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// settingsResp mirrors the bespoke Settings panel JSON shape.
type settingsResp struct {
	Rows []struct {
		Group string `json:"group"`
		Label string `json:"label"`
		Value string `json:"value"`
		Note  string `json:"note"`
	} `json:"rows"`
	ManageCmd string `json:"manageCmd"`
	Note      string `json:"note"`
}

func ptrStrSlice(v []string) *[]string { return &v }

func clusterSettings() *spiceboxv1alpha1.ClusterAgentSettings {
	cs := &spiceboxv1alpha1.ClusterAgentSettings{}
	cs.Name = spiceboxv1alpha1.ClusterAgentSettingsName // "cluster"
	allowModelOverride := false
	cs.Spec.Limits = &spiceboxv1alpha1.SettingsLimits{
		Budget: &spiceboxv1alpha1.SettingsBudgetCeiling{
			MaxTurns:    50,
			MaxTokens:   1000000,
			MaxDuration: metav1.Duration{},
		},
		DeniedModels:       []string{"test-model-denied"},
		AllowModelOverride: &allowModelOverride,
		AllowedSkills:      ptrStrSlice([]string{}), // non-nil empty → deny-all
		DeniedSkills:       []string{"github.com/evil/**"},
	}
	cs.Spec.Defaults = &spiceboxv1alpha1.SettingsDefaults{
		Model: &spiceboxv1alpha1.DefaultModel{Provider: "anthropic", Name: "claude-sonnet"},
	}
	cs.Spec.ModelCatalog = &[]spiceboxv1alpha1.ModelCatalogEntry{
		{Name: "test-model-a", Provider: "anthropic", Default: true, InputPerMTok: 3, OutputPerMTok: 15},
		{Name: "test-model-b", Provider: "anthropic"},
	}
	return cs
}

func rowValue(t *testing.T, rows settingsResp, group, labelSub string) (string, bool) {
	t.Helper()
	for _, r := range rows.Rows {
		if r.Group == group && strings.Contains(r.Label, labelSub) {
			return r.Value, true
		}
	}
	return "", false
}

func TestAdmindConfigSettings(t *testing.T) {
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(clusterSettings()).Build()
	a := newTestAdmind(t, k8s)
	h := a.Handler()

	// view_config gates it: a non-admin subject is forbidden.
	w := do(t, h, http.MethodGet, "/admin/v1/config/settings", "test-token", "user:bm9wZQ", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "settings needs view_config")

	w = do(t, h, http.MethodGet, "/admin/v1/config/settings", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	var resp settingsResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Rows, "configured ClusterAgentSettings yields rows")
	assert.Empty(t, resp.Note, "configured settings: no not-found note")
	assert.NotEmpty(t, resp.ManageCmd, "manage command shown")

	v, ok := rowValue(t, resp, "Limits", "Max Tokens")
	require.True(t, ok, "Limits Max Tokens row present")
	assert.Equal(t, "1000000", v)

	v, ok = rowValue(t, resp, "Limits", "Max Turns")
	require.True(t, ok, "Limits Max Turns row present")
	assert.Equal(t, "50", v)

	v, ok = rowValue(t, resp, "Limits", "Denied Models")
	require.True(t, ok, "Limits Denied Models row present")
	assert.Contains(t, v, "test-model-denied")

	v, ok = rowValue(t, resp, "Limits", "Allow Model Override")
	require.True(t, ok, "Limits Allow Model Override row present")
	assert.Contains(t, v, "false")

	v, ok = rowValue(t, resp, "Limits", "Model Catalog · test-model-a")
	require.True(t, ok, "Model Catalog row present for test-model-a")
	assert.Contains(t, v, "anthropic")
	assert.Contains(t, v, "$3.00 in / $15.00 out per MTok")

	v, ok = rowValue(t, resp, "Limits", "Model Catalog · test-model-b")
	require.True(t, ok, "Model Catalog row present for test-model-b")
	assert.Contains(t, v, "no price")

	// non-nil empty allowlist → deny-all (tri-state pointer semantics).
	v, ok = rowValue(t, resp, "Limits", "Allowed Skills")
	require.True(t, ok, "Limits Allowed Skills row present (non-nil empty)")
	assert.Contains(t, strings.ToLower(v), "deny", "empty allowlist renders deny-all")

	_, ok = rowValue(t, resp, "Limits", "Denied Skills")
	assert.True(t, ok, "Denied Skills row present")

	v, ok = rowValue(t, resp, "Defaults", "Model")
	require.True(t, ok, "Defaults Model row present")
	assert.Contains(t, v, "anthropic")
	assert.Contains(t, v, "claude-sonnet")
}

func TestAdmindConfigSettings_NotFound(t *testing.T) {
	// No ClusterAgentSettings in the cluster.
	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/config/settings", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code, "missing settings → 200, not 404/500")

	var resp settingsResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Rows, "no settings → empty rows")
	assert.NotEmpty(t, resp.Note, "no settings → explanatory note")
	assert.Contains(t, strings.ToLower(resp.Note), "no clusteragentsettings")
}

// TestAdmindConfigSettingsRoutePrecedence proves the bespoke
// /admin/v1/config/settings handler wins over the generic
// /admin/v1/config/{resource} projector route (net/http ServeMux dispatches
// the more specific pattern first). A "settings"-slug projector is registered
// with a sentinel row that must NOT appear in the response.
func TestAdmindConfigSettingsRoutePrecedence(t *testing.T) {
	config.Reset()
	t.Cleanup(config.Reset)
	const sentinel = "PROJECTOR-SHOULD-NOT-WIN"
	config.Register(stubProjector{resource: "settings", rows: []config.ResourceRow{
		{Name: sentinel, Scope: "cluster", Status: "Valid"},
	}})

	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/config/settings", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), sentinel,
		"the generic projector must NOT serve /config/settings — bespoke handler wins")

	// And the body must be the bespoke settings shape (has manageCmd / note),
	// not a bare ResourceRow array.
	var resp settingsResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "body is the settings object shape")
	assert.NotEmpty(t, resp.Note, "not-found settings note present, confirming the bespoke handler ran")
}
