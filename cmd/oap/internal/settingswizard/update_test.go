package settingswizard

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// loadCAS reads back the singleton for post-condition assertions.
func loadCAS(t *testing.T, c client.Client) *v1alpha1.ClusterAgentSettings {
	t.Helper()
	got, err := LoadExisting(context.Background(), c)
	require.NoError(t, err)
	return got
}

func TestUpdateDefaultModelEntry(t *testing.T) {
	t.Run("existing default entry updated in place, other entries + limits preserved", func(t *testing.T) {
		limits := &v1alpha1.SettingsLimits{
			Pinning: &v1alpha1.PinningPolicy{Rules: []v1alpha1.PinningRule{
				{Kind: "image", MinStrength: "digest", Mode: v1alpha1.PinModeBlock},
			}},
		}
		existing := &v1alpha1.ClusterAgentSettings{
			ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ClusterAgentSettingsName},
			Spec: v1alpha1.SettingsSpec{
				Limits: limits,
				ModelCatalog: &[]v1alpha1.ModelCatalogEntry{
					{
						Name: "claude-opus", Provider: "anthropic", Default: true,
						TokenRef: &v1alpha1.NamespacedSecretKeyRef{
							Namespace: "agentprimitives-system", Name: "model-default-token", Key: "token",
						},
						InputPerMTok: 15,
					},
					{
						Name: "gpt-extra", Provider: "openai",
						TokenRef: &v1alpha1.NamespacedSecretKeyRef{
							Namespace: "agentprimitives-system", Name: "model-default-token-gpt-extra", Key: "token",
						},
					},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()

		require.NoError(t, UpdateDefaultModelEntry(context.Background(), c, "openai", "gpt-5"))

		got := loadCAS(t, c)
		require.NotNil(t, got.Spec.ModelCatalog)
		cat := *got.Spec.ModelCatalog
		require.Len(t, cat, 2, "no entry added or dropped")

		def := cat[0]
		assert.True(t, def.Default)
		assert.Equal(t, "openai", def.Provider, "provider overwritten in place")
		assert.Equal(t, "gpt-5", def.Name, "name overwritten in place")
		require.NotNil(t, def.TokenRef)
		assert.Equal(t, "model-default-token", def.TokenRef.Name, "the default's TokenRef is preserved")
		assert.EqualValues(t, 15, def.InputPerMTok, "other fields on the default entry are preserved")

		other := cat[1]
		assert.Equal(t, "gpt-extra", other.Name, "the non-default entry survives untouched")
		assert.False(t, other.Default)

		require.NotNil(t, got.Spec.Limits)
		require.NotNil(t, got.Spec.Limits.Pinning)
		require.Len(t, got.Spec.Limits.Pinning.Rules, 1, "the pinning ceiling survives")
		assert.Equal(t, v1alpha1.PinModeBlock, got.Spec.Limits.Pinning.Rules[0].Mode)
	})

	t.Run("no default entry: a default is appended pointing at the central token Secret", func(t *testing.T) {
		existing := &v1alpha1.ClusterAgentSettings{
			ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ClusterAgentSettingsName},
			Spec: v1alpha1.SettingsSpec{
				ModelCatalog: &[]v1alpha1.ModelCatalogEntry{
					{Name: "gpt-extra", Provider: "openai"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()

		require.NoError(t, UpdateDefaultModelEntry(context.Background(), c, "anthropic", "claude-sonnet-5"))

		got := loadCAS(t, c)
		require.NotNil(t, got.Spec.ModelCatalog)
		cat := *got.Spec.ModelCatalog
		require.Len(t, cat, 2, "the pre-existing entry survives and a default is appended")
		assert.Equal(t, "gpt-extra", cat[0].Name)
		assert.False(t, cat[0].Default)

		appended := cat[1]
		assert.True(t, appended.Default)
		assert.Equal(t, "anthropic", appended.Provider)
		assert.Equal(t, "claude-sonnet-5", appended.Name)
		require.NotNil(t, appended.TokenRef)
		assert.Equal(t, "agentprimitives-system", appended.TokenRef.Namespace)
		assert.Equal(t, "model-default-token", appended.TokenRef.Name)
		assert.Equal(t, "token", appended.TokenRef.Key)
	})

	t.Run("no CR at all: the singleton is created carrying just the default entry", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

		require.NoError(t, UpdateDefaultModelEntry(context.Background(), c, "anthropic", "claude-opus-4-8"))

		got := loadCAS(t, c)
		require.NotNil(t, got.Spec.ModelCatalog)
		cat := *got.Spec.ModelCatalog
		require.Len(t, cat, 1)
		assert.True(t, cat[0].Default)
		assert.Equal(t, "anthropic", cat[0].Provider)
		assert.Equal(t, "claude-opus-4-8", cat[0].Name)
		require.NotNil(t, cat[0].TokenRef)
		assert.Equal(t, "model-default-token", cat[0].TokenRef.Name)
	})
}
