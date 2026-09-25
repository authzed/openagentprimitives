//go:build integration

// pkg/apis/v1alpha1/agentclass_validation_test.go
//
// Integration test for the AgentClassSpec CEL validations gating
// identityMode=ask|dynamic:
//   - ask|dynamic requires spec.agentIdentity to be set.
//   - dynamic additionally requires spec.identityRecommendation.prompt.
//
// Runs against a real envtest apiserver (via pkg/controllers/testenv) so the
// assertion exercises the generated CRD's XValidation rules directly, not a
// Go-level pre-check that could drift from what's actually installed.
package v1alpha1_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// newIdentityModeClass builds a minimally-valid AgentClass (a systemPrompt is
// the only spec-required field) with the given name and identityMode, for
// exercising the ask|dynamic CEL gates in isolation.
func newIdentityModeClass(name, mode string) *v1alpha1.AgentClass {
	return &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.AgentClassSpec{
			SystemPrompt: v1alpha1.PromptSource{Inline: "you are an agent"},
			IdentityMode: mode,
		},
	}
}

func TestAgentClassIdentityModeCEL(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	t.Run("ask without agentIdentity: rejected", func(t *testing.T) {
		class := newIdentityModeClass("ask-no-identity", v1alpha1.IdentityModeAsk)
		err := env.Client.Create(ctx, class)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
		assert.Contains(t, err.Error(), "spec.identityMode ask|dynamic requires spec.agentIdentity to be set")
	})

	t.Run("dynamic without agentIdentity: rejected", func(t *testing.T) {
		class := newIdentityModeClass("dynamic-no-identity", v1alpha1.IdentityModeDynamic)
		err := env.Client.Create(ctx, class)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
		assert.Contains(t, err.Error(), "spec.identityMode ask|dynamic requires spec.agentIdentity to be set")
	})

	t.Run("dynamic with agentIdentity but no identityRecommendation.prompt: rejected", func(t *testing.T) {
		class := newIdentityModeClass("dynamic-no-prompt", v1alpha1.IdentityModeDynamic)
		class.Spec.AgentIdentity = "my-identity"
		err := env.Client.Create(ctx, class)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
		assert.Contains(t, err.Error(), "spec.identityMode=dynamic requires spec.identityRecommendation.prompt")
	})

	t.Run("well-formed ask: accepted", func(t *testing.T) {
		class := newIdentityModeClass("ask-well-formed", v1alpha1.IdentityModeAsk)
		class.Spec.AgentIdentity = "my-identity"
		assert.NoError(t, env.Client.Create(ctx, class))
	})

	t.Run("well-formed dynamic: accepted", func(t *testing.T) {
		class := newIdentityModeClass("dynamic-well-formed", v1alpha1.IdentityModeDynamic)
		class.Spec.AgentIdentity = "my-identity"
		class.Spec.IdentityRecommendation = &v1alpha1.IdentityRecommendationConfig{
			Prompt: "prefer the user's own identity in single-user threads",
		}
		assert.NoError(t, env.Client.Create(ctx, class))
	})

	t.Run("static modes unaffected: agent without agentIdentity accepted", func(t *testing.T) {
		class := newIdentityModeClass("agent-mode-no-identity", v1alpha1.IdentityModeAgent)
		assert.NoError(t, env.Client.Create(ctx, class))
	})
}
