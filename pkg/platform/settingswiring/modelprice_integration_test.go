//go:build integration

// pkg/platform/settingswiring/modelprice_integration_test.go
package settingswiring

import (
	"context"
	"fmt"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// newModelPriceClass builds a minimal AgentClass whose model resolves via the
// cluster model catalog: Model.FromCatalog with no inline apiKey takes
// resolveModel's catalog branch, the only branch that carries a price through to
// EffectiveSettings (see pkg/platform/settings/modelprice_test.go).
func newModelPriceClass(namespace, name, fromCatalog string) *v1.AgentClass {
	return &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1.AgentClassSpec{
			Model:        &v1.ModelConfig{FromCatalog: fromCatalog},
			SystemPrompt: v1.PromptSource{Inline: "you are an agent"},
			Budget:       &v1.BudgetConfig{MaxTurns: 10, MaxTokens: 50000},
		},
	}
}

// TestModelPriceResolution_CatalogAuthoritative is the CRD-backed half of the
// catalog-authoritative guarantee (pkg/platform/settings/modelprice_test.go is
// the pure-function half): through a real envtest apiserver, a
// ClusterAgentSettings.spec.modelCatalog price must flow through
// ResolveForSession's live FetchTiers Get into
// EffectiveSettings.ModelInputPerMTok/ModelOutputPerMTok — the same value the
// runner's cost hook (pkg/agent/postsession/cost) and the admin dashboard read.
func TestModelPriceResolution_CatalogAuthoritative(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	cas := &v1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterAgentSettingsName},
		Spec: v1.SettingsSpec{
			ModelCatalog: &[]v1.ModelCatalogEntry{
				{
					Name: "claude-opus-4-8", Provider: "anthropic",
					TokenRef:      &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "anthropic-key", Key: "token"},
					InputPerMTok:  7,
					OutputPerMTok: 35,
				},
				{
					// No price: proves the resolver stamps 0/0 rather than
					// fabricating a figure for a catalog entry that carries none.
					Name: "claude-haiku-4-5", Provider: "anthropic",
					TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "cheap-key", Key: "token"},
				},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cas), "create ClusterAgentSettings")
	t.Cleanup(func() { _ = env.Client.Delete(ctx, cas) })

	cases := []struct {
		name        string
		fromCatalog string
		wantIn      float64
		wantOut     float64
	}{
		{"priced catalog entry: EffectiveSettings carries its InputPerMTok/OutputPerMTok", "claude-opus-4-8", 7, 35},
		{"unpriced catalog entry: EffectiveSettings carries 0/0, not a fabricated price", "claude-haiku-4-5", 0, 0},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := newModelPriceClass("default", fmt.Sprintf("mp-class-%d", i), tc.fromCatalog)
			require.NoError(t, env.Client.Create(ctx, class), "create AgentClass")
			t.Cleanup(func() { _ = env.Client.Delete(ctx, class) })

			sess := &v1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: fmt.Sprintf("mp-sess-%d", i)},
				Spec: v1.AgentSessionSpec{
					Class:  class.Name,
					Prompt: v1.PromptSource{Inline: "hello"},
				},
			}
			require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
			t.Cleanup(func() { _ = env.Client.Delete(ctx, sess) })

			eff, vs, err := ResolveForSession(ctx, env.Client, class, sess)
			require.NoError(t, err, "ResolveForSession")
			assert.False(t, HasFatal(vs), "well-formed catalog model must not produce a fatal violation: %+v", vs)
			assert.Equal(t, tc.wantIn, eff.ModelInputPerMTok, "ModelInputPerMTok must reflect the resolved catalog entry")
			assert.Equal(t, tc.wantOut, eff.ModelOutputPerMTok, "ModelOutputPerMTok must reflect the resolved catalog entry")
		})
	}
}
