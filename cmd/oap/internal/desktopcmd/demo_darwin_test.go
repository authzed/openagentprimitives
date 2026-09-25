//go:build darwin && arm64

package desktopcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/demo"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestDemoAgentClassYAML_MatchesChatNamespace pins the embedded demo
// AgentClass (cmd/oap/internal/desktop/demo/pirate.yaml — the `pirate-private`
// pirate-speak translator) to the exact name/namespace applyDemoAgentClass
// requires, and sanity-checks
// it actually carries a non-empty system prompt (the whole reason the demo
// agent exists — an empty prompt would mean the chat's agent selector has
// something to click but nothing for the model to do).
func TestDemoAgentClassYAML_MatchesChatNamespace(t *testing.T) {
	var ac spiceboxv1alpha1.AgentClass
	require.NoError(t, yaml.Unmarshal(demo.AgentClassYAML, &ac))

	assert.Equal(t, demoAgentClassName, ac.Name)
	assert.Equal(t, demoChatNamespace, ac.Namespace)
	assert.NotEmpty(t, ac.Spec.SystemPrompt.Inline, "demo AgentClass must carry an inline system prompt")
}

func newDemoAgentClassScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

func TestApplyDemoAgentClass(t *testing.T) {
	ctx := context.Background()

	t.Run("no existing AgentClass -> created from the embedded YAML", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newDemoAgentClassScheme(t)).Build()

		require.NoError(t, applyDemoAgentClass(ctx, c))

		var got spiceboxv1alpha1.AgentClass
		require.NoError(t, c.Get(ctx, types.NamespacedName{Name: demoAgentClassName, Namespace: demoChatNamespace}, &got))
		assert.NotEmpty(t, got.Spec.SystemPrompt.Inline)
	})

	t.Run("existing AgentClass with a stale spec -> spec updated in place", func(t *testing.T) {
		existing := &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: demoAgentClassName, Namespace: demoChatNamespace},
			Spec: spiceboxv1alpha1.AgentClassSpec{
				Description:  "stale description from a previous oap binary",
				SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "stale prompt"},
			},
		}
		c := fake.NewClientBuilder().WithScheme(newDemoAgentClassScheme(t)).WithObjects(existing).Build()

		require.NoError(t, applyDemoAgentClass(ctx, c))

		var got spiceboxv1alpha1.AgentClass
		require.NoError(t, c.Get(ctx, types.NamespacedName{Name: demoAgentClassName, Namespace: demoChatNamespace}, &got))
		assert.NotEqual(t, "stale prompt", got.Spec.SystemPrompt.Inline, "Update must replace the stale spec with the embedded one")
		assert.Contains(t, got.Spec.SystemPrompt.Inline, "pirate")
	})
}
