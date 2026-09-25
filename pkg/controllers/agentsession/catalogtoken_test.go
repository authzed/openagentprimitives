package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestBuildRunnerRBAC_IncludesLLMAPIKey pins the contract that a
// non-empty llmAPIKey is written into the per-session Secret under
// agentSessionSecretLLMAPIKey, so the runner pod's SubPath mount
// delivers the token without any changes to podspec.go.
func TestBuildRunnerRBAC_IncludesLLMAPIKey(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "ns1"},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns1"},
	}

	_, _, _, sec, _ := BuildRunnerRBAC(sess, ac, "tok", nil, nil, "", "spicedb", "argskey", "auditseed", "sk-test-123", "", nil)

	require.NotNil(t, sec, "per-session secret must be built")
	assert.Equal(t, "sk-test-123", string(sec.Data[agentSessionSecretLLMAPIKey]),
		"llm-api-key must be written into the per-session Secret when a non-empty llmAPIKey is passed")
}

// TestMaterializeCatalogToken covers the happy path (Secret present with the
// expected key), the missing-Secret error path, and the present-key-but-empty-
// value error path (the impl guards len(v)==0 so an emptied token isn't
// silently treated as valid). Uses a fake client seeded with two Secrets in
// agentprimitives-system.
func TestMaterializeCatalogToken(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "catalog-token",
				Namespace: "agentprimitives-system",
			},
			Data: map[string][]byte{"token": []byte("sk-test-xyz")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "empty-token",
				Namespace: "agentprimitives-system",
			},
			Data: map[string][]byte{"token": {}},
		},
	).Build()
	r := &Reconciler{Client: c}

	got, err := r.materializeCatalogToken(context.Background(),
		&spiceboxv1alpha1.NamespacedSecretKeyRef{
			Namespace: "agentprimitives-system",
			Name:      "catalog-token",
			Key:       "token",
		})
	require.NoError(t, err)
	assert.Equal(t, "sk-test-xyz", got)

	_, err = r.materializeCatalogToken(context.Background(),
		&spiceboxv1alpha1.NamespacedSecretKeyRef{
			Namespace: "agentprimitives-system",
			Name:      "missing-secret",
			Key:       "token",
		})
	assert.Error(t, err, "missing secret must error, not silently return empty")

	_, err = r.materializeCatalogToken(context.Background(),
		&spiceboxv1alpha1.NamespacedSecretKeyRef{
			Namespace: "agentprimitives-system",
			Name:      "empty-token",
			Key:       "token",
		})
	assert.Error(t, err, "key present but empty value must error, not silently return empty")
}
