package settings

import (
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
)

// TestResolve_ModelPrice_CatalogAuthoritative verifies the resolved catalog
// entry's price flows through Resolve into EffectiveSettings, so the runner's
// cost hook can prefer it over the provider's built-in table (matching the
// admin dashboard, which is catalog-authoritative).
func TestResolve_ModelPrice_CatalogAuthoritative(t *testing.T) {
	pricedEntry := v1.ModelCatalogEntry{
		Name: "claude-opus-4-8", Provider: "anthropic",
		TokenRef:      &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "anthropic-key", Key: "token"},
		InputPerMTok:  7,
		OutputPerMTok: 35,
	}
	unpricedEntry := v1.ModelCatalogEntry{
		Name: "claude-haiku-4-5", Provider: "anthropic",
		TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "cheap-key", Key: "token"},
	}

	cases := []struct {
		name    string
		model   string
		wantIn  float64
		wantOut float64
	}{
		{"catalog entry with a price stamps EffectiveSettings", "claude-opus-4-8", 7, 35},
		{"catalog entry with no price stamps 0/0", "claude-haiku-4-5", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := Inputs{
				Cluster:    clusterWithCatalog([]v1.ModelCatalogEntry{pricedEntry, unpricedEntry}, nil, nil),
				ClassModel: &v1.ModelConfig{FromCatalog: tc.model},
				ForSession: true,
			}
			got, vs := Resolve(in)
			assert.Empty(t, firstFatalReason(vs), "no fatal expected")
			assert.Equal(t, tc.wantIn, got.ModelInputPerMTok)
			assert.Equal(t, tc.wantOut, got.ModelOutputPerMTok)
		})
	}
}

// TestResolveModel_Price_NonCatalogPathsAreZero covers the paths that must
// NOT carry a price: legacy (no catalog) and BYO-override. Both return the
// zero value regardless of what the catalog might otherwise say.
func TestResolveModel_Price_NonCatalogPathsAreZero(t *testing.T) {
	t.Run("legacy path (no catalog set) is unpriced", func(t *testing.T) {
		in := Inputs{
			ClassModel: &v1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-8", APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}},
			ForSession: true,
		}
		_, _, inPrice, outPrice, _, vs := resolveModel(in, map[string]string{})
		assert.Empty(t, firstFatalReason(vs))
		assert.Zero(t, inPrice)
		assert.Zero(t, outPrice)
	})

	t.Run("BYO override path is unpriced even though the catalog entry has a price", func(t *testing.T) {
		priced := v1.ModelCatalogEntry{
			Name: "claude-opus-4-8", Provider: "anthropic",
			TokenRef:      &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "anthropic-key", Key: "token"},
			InputPerMTok:  7,
			OutputPerMTok: 35,
		}
		in := Inputs{
			Cluster:    clusterWithCatalog([]v1.ModelCatalogEntry{priced}, nil, boolPtr(true)),
			ClassModel: &v1.ModelConfig{Provider: "anthropic", Name: "byo-model", APIKey: v1.SecretKeyRef{Name: "byo", Key: "k"}},
			ForSession: true,
		}
		_, _, inPrice, outPrice, _, vs := resolveModel(in, map[string]string{})
		assert.Empty(t, firstFatalReason(vs))
		assert.Zero(t, inPrice)
		assert.Zero(t, outPrice)
	})
}
