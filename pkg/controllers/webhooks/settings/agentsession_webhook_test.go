package settings

import (
	"context"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestAgentSessionWebhook_DisallowedModel_Denied(t *testing.T) {
	// Cluster catalog contains only claude-opus-4-8 and denies claude-haiku-4-5;
	// class uses claude-haiku-4-5 → ResolveForSession should produce a fatal violation.
	cluster := &v1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterAgentSettingsName},
		Spec: v1.SettingsSpec{
			ModelCatalog: &[]v1.ModelCatalogEntry{{
				Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
				TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
			}},
			Limits: &v1.SettingsLimits{DeniedModels: []string{"claude-haiku-4-5"}},
		},
	}
	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			Model:  &v1.ModelConfig{Provider: "anthropic", Name: "claude-haiku-4-5", APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}},
			Budget: &v1.BudgetConfig{MaxTurns: 5, MaxTokens: 1000},
		},
	}
	sess := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sess-1"},
		Spec:       v1.AgentSessionSpec{Class: "bot"},
	}

	sc := scheme()
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(cluster, class).Build()
	h := NewAgentSessionWebhook(c, admission.NewDecoder(sc))

	resp := h.Handle(context.Background(), reqFor(t, sess))
	assert.False(t, resp.Allowed)
	assert.Contains(t, resp.Result.Message, "claude-haiku-4-5")
}

func TestAgentSessionWebhook_AllowedModel_Allowed(t *testing.T) {
	// No settings → unconstrained; model should resolve fine.
	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			Model:  &v1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-8", APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}},
			Budget: &v1.BudgetConfig{MaxTurns: 5, MaxTokens: 1000},
		},
	}
	sess := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sess-2"},
		Spec:       v1.AgentSessionSpec{Class: "bot"},
	}

	sc := scheme()
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(class).Build()
	h := NewAgentSessionWebhook(c, admission.NewDecoder(sc))

	resp := h.Handle(context.Background(), reqFor(t, sess))
	assert.True(t, resp.Allowed)
}

func TestAgentSessionWebhook_ClassNotFound_AllowedFailOpen(t *testing.T) {
	// Session references a non-existent class → fail open (controller's gate).
	sess := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sess-3"},
		Spec:       v1.AgentSessionSpec{Class: "missing-class"},
	}

	sc := scheme()
	c := fake.NewClientBuilder().WithScheme(sc).Build()
	h := NewAgentSessionWebhook(c, admission.NewDecoder(sc))

	resp := h.Handle(context.Background(), reqFor(t, sess))
	assert.True(t, resp.Allowed, "class-not-found must fail open")
	require.NotNil(t, resp.Result)
	assert.Contains(t, resp.Result.Message, "class not found")
}
