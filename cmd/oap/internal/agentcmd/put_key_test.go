package agentcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newAgentPutKeyScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, fn := range []func(*runtime.Scheme) error{corev1.AddToScheme, spiceboxv1alpha1.AddToScheme} {
		require.NoError(t, fn(s), "AddToScheme")
	}
	return s
}

func makeAgentClass(ns, name, secretName, secretKey string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-5-sonnet-20241022",
				APIKey: spiceboxv1alpha1.SecretKeyRef{
					Name: secretName,
					Key:  secretKey,
				},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are helpful"},
		},
	}
}

func TestAgentPutKey_HappyCreate(t *testing.T) {
	ctx := context.Background()
	scheme := newAgentPutKeyScheme(t)
	ac := makeAgentClass("ns", "my-agent", "my-agent-key", "ANTHROPIC_API_KEY")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ac).Build()

	secretName, secretKey, created, err := storeAgentClassKey(ctx, c, "ns", "my-agent", []byte("sk-abc123"))
	require.NoError(t, err, "storeAgentClassKey")
	assert.True(t, created, "expected created=true for new Secret")
	assert.Equal(t, "my-agent-key", secretName, "returned secretName")
	assert.Equal(t, "ANTHROPIC_API_KEY", secretKey, "returned secretKey")

	var sec corev1.Secret
	require.NoError(t,
		c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "my-agent-key"}, &sec),
		"get created Secret")
	assert.Equal(t, "sk-abc123", string(sec.Data["ANTHROPIC_API_KEY"]), "Secret data")
}

func TestAgentPutKey_HappyUpdate(t *testing.T) {
	ctx := context.Background()
	scheme := newAgentPutKeyScheme(t)
	ac := makeAgentClass("ns", "my-agent", "my-agent-key", "ANTHROPIC_API_KEY")
	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-agent-key", Namespace: "ns"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"ANTHROPIC_API_KEY": []byte("old-key")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ac, existingSecret).Build()

	_, _, created, err := storeAgentClassKey(ctx, c, "ns", "my-agent", []byte("new-key"))
	require.NoError(t, err, "storeAgentClassKey")
	assert.False(t, created, "expected created=false for existing Secret")

	var sec corev1.Secret
	require.NoError(t,
		c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "my-agent-key"}, &sec),
		"get updated Secret")
	assert.Equal(t, "new-key", string(sec.Data["ANTHROPIC_API_KEY"]), "Secret data rotated")
}

func TestAgentPutKey_MissingAgentClass(t *testing.T) {
	ctx := context.Background()
	scheme := newAgentPutKeyScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, _, _, err := storeAgentClassKey(ctx, c, "ns", "nonexistent", []byte("sk-abc"))
	require.Error(t, err, "missing AgentClass should error")
	assert.Contains(t, err.Error(), "get AgentClass", "error should mention 'get AgentClass'")
}

func TestAgentPutKey_MissingAPIKeyRef(t *testing.T) {
	ctx := context.Background()
	scheme := newAgentPutKeyScheme(t)
	// AgentClass with empty apiKey SecretKeyRef.
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "no-apikey", Namespace: "ns"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-5-sonnet-20241022",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{}, // empty
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are helpful"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ac).Build()

	_, _, _, err := storeAgentClassKey(ctx, c, "ns", "no-apikey", []byte("sk-abc"))
	require.Error(t, err, "missing apiKey config should error")
	assert.Contains(t, err.Error(), "spec.model.apiKey is not configured", "error should describe missing apiKey")
}
