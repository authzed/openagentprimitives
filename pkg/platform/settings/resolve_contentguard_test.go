package settings

import (
	"testing"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
)

func TestResolve_ContentInspectors_UnionAcrossTiers(t *testing.T) {
	cluster := &v1.SettingsSpec{Limits: &v1.SettingsLimits{
		ContentInspectors: &[]v1.ContentInspectorConfig{
			{ID: "url-allowlist", Config: apiextv1.JSON{}},
		},
	}}
	ns := &v1.SettingsSpec{Limits: &v1.SettingsLimits{
		ContentInspectors: &[]v1.ContentInspectorConfig{
			{ID: "pii-scanner", Config: apiextv1.JSON{}},
		},
	}}
	eff, _ := Resolve(Inputs{Cluster: cluster, Namespace: ns})
	var ids []string
	for _, ci := range eff.ContentInspectors {
		ids = append(ids, ci.ID)
	}
	assert.ElementsMatch(t, []string{"url-allowlist", "pii-scanner"}, ids,
		"both tiers' inspectors must be present (union); lower tier cannot remove cluster's")
}

func TestResolve_ContentInspectors_NilSafe(t *testing.T) {
	// nil Limits → contributes nothing; no panic
	cluster := &v1.SettingsSpec{}
	ns := &v1.SettingsSpec{Limits: &v1.SettingsLimits{
		ContentInspectors: &[]v1.ContentInspectorConfig{
			{ID: "pii-scanner", Config: apiextv1.JSON{}},
		},
	}}
	eff, _ := Resolve(Inputs{Cluster: cluster, Namespace: ns})
	var ids []string
	for _, ci := range eff.ContentInspectors {
		ids = append(ids, ci.ID)
	}
	assert.ElementsMatch(t, []string{"pii-scanner"}, ids,
		"only namespace tier contributed; nil cluster Limits must be skipped")
}

func TestResolve_ContentInspectors_AllNil(t *testing.T) {
	// no tier sets ContentInspectors → empty slice in effective
	eff, _ := Resolve(Inputs{Cluster: &v1.SettingsSpec{}, Namespace: nil})
	assert.Empty(t, eff.ContentInspectors, "no tier set ContentInspectors; result must be empty")
}
