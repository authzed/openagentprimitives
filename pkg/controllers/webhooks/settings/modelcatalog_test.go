package settings

import (
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
)

func entries(es ...v1.ModelCatalogEntry) *[]v1.ModelCatalogEntry { return &es }

func TestModelCatalogError(t *testing.T) {
	tokenRef := &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"}
	cases := []struct {
		name      string
		spec      v1.SettingsSpec
		isCluster bool
		wantErr   bool
	}{
		{name: "cluster: valid single default with tokenRef", isCluster: true,
			spec: v1.SettingsSpec{ModelCatalog: entries(v1.ModelCatalogEntry{Name: "m", Provider: "anthropic", TokenRef: tokenRef, Default: true})}},
		{name: "cluster: entry missing tokenRef is rejected", isCluster: true, wantErr: true,
			spec: v1.SettingsSpec{ModelCatalog: entries(v1.ModelCatalogEntry{Name: "m", Provider: "anthropic"})}},
		{name: "cluster: two defaults rejected", isCluster: true, wantErr: true,
			spec: v1.SettingsSpec{ModelCatalog: entries(
				v1.ModelCatalogEntry{Name: "a", Provider: "anthropic", TokenRef: tokenRef, Default: true},
				v1.ModelCatalogEntry{Name: "b", Provider: "anthropic", TokenRef: tokenRef, Default: true})}},
		{name: "cluster: duplicate names rejected", isCluster: true, wantErr: true,
			spec: v1.SettingsSpec{ModelCatalog: entries(
				v1.ModelCatalogEntry{Name: "a", Provider: "anthropic", TokenRef: tokenRef},
				v1.ModelCatalogEntry{Name: "a", Provider: "anthropic", TokenRef: tokenRef})}},
		{name: "cluster: default also denied rejected", isCluster: true, wantErr: true,
			spec: v1.SettingsSpec{
				ModelCatalog: entries(v1.ModelCatalogEntry{Name: "a", Provider: "anthropic", TokenRef: tokenRef, Default: true}),
				Limits:       &v1.SettingsLimits{DeniedModels: []string{"a"}}}},
		{name: "namespace: tokenRef rejected", isCluster: false, wantErr: true,
			spec: v1.SettingsSpec{ModelCatalog: entries(v1.ModelCatalogEntry{Name: "m", TokenRef: tokenRef})}},
		{name: "namespace: name-only narrowing ok", isCluster: false,
			spec: v1.SettingsSpec{ModelCatalog: entries(v1.ModelCatalogEntry{Name: "m"})}},
		{name: "no catalog is ok", isCluster: true, spec: v1.SettingsSpec{}},
		{name: "cluster: empty-name entry rejected", isCluster: true, wantErr: true,
			spec: v1.SettingsSpec{ModelCatalog: entries(v1.ModelCatalogEntry{Name: "", Provider: "anthropic", TokenRef: tokenRef, Default: true})}},
		{name: "namespace: default rejected", isCluster: false, wantErr: true,
			spec: v1.SettingsSpec{ModelCatalog: entries(v1.ModelCatalogEntry{Name: "m", Default: true})}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ModelCatalogError(&tc.spec, tc.isCluster)
			if tc.wantErr {
				assert.NotEmpty(t, got)
			} else {
				assert.Empty(t, got)
			}
		})
	}
}
