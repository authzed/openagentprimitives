package settings

import (
	"context"
	"encoding/json"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// scheme builds a runtime.Scheme with the agentprimitives API types registered.
// Shared by all tests in this package.
func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = v1.AddToScheme(s)
	return s
}

// reqFor marshals obj into a bare admission.Request. The object's GVK does not
// need to be set — the decoder uses the concrete type the handler passes in.
func reqFor(t *testing.T, obj runtime.Object) admission.Request {
	t.Helper()
	raw, err := json.Marshal(obj)
	require.NoError(t, err)
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Object: runtime.RawExtension{Raw: raw},
		},
	}
}

func TestAgentClassWebhook_DisallowedModel_Denied(t *testing.T) {
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
	sc := scheme()
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(cluster).Build()
	h := NewAgentClassWebhook(c, admission.NewDecoder(sc))

	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			Model:  &v1.ModelConfig{Provider: "anthropic", Name: "claude-haiku-4-5", APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}},
			Budget: &v1.BudgetConfig{MaxTurns: 1, MaxTokens: 1},
		},
	}
	resp := h.Handle(context.Background(), reqFor(t, class))
	assert.False(t, resp.Allowed)
	assert.Contains(t, resp.Result.Message, "claude-haiku-4-5")
}

func TestAgentClassWebhook_AllowedModel_Allowed(t *testing.T) {
	sc := scheme()
	// No settings objects → unconstrained.
	c := fake.NewClientBuilder().WithScheme(sc).Build()
	h := NewAgentClassWebhook(c, admission.NewDecoder(sc))

	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			Model:  &v1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-8", APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}},
			Budget: &v1.BudgetConfig{MaxTurns: 1, MaxTokens: 1},
		},
	}
	resp := h.Handle(context.Background(), reqFor(t, class))
	assert.True(t, resp.Allowed)
}

func TestAgentClassWebhook_NoSettings_Allowed(t *testing.T) {
	sc := scheme()
	c := fake.NewClientBuilder().WithScheme(sc).Build()
	h := NewAgentClassWebhook(c, admission.NewDecoder(sc))

	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			Model:  &v1.ModelConfig{Provider: "anthropic", Name: "any-model", APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}},
			Budget: &v1.BudgetConfig{MaxTurns: 5, MaxTokens: 1000},
		},
	}
	resp := h.Handle(context.Background(), reqFor(t, class))
	assert.True(t, resp.Allowed, "no settings means unconstrained — must allow")
}
