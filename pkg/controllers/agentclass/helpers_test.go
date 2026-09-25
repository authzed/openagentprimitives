// pkg/controllers/agentclass/helpers_test.go
//
// Untagged test helpers shared by both the unit-tier (binding_validation_test,
// skills_validation_test, fact_sources_test, interact_permission_derivation_test)
// and the integration-tier (controller_test).
// No build tag — must compile in all build configurations.
package agentclass_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

func newClass(name string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-opus-4-7",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    50,
				MaxTokens:   100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
		},
	}
}

func findCondition(cs []metav1.Condition, t string) *metav1.Condition {
	for i := range cs {
		if cs[i].Type == t {
			return &cs[i]
		}
	}
	return nil
}

// reconcileClass builds a fake cluster from ac plus objs, reconciles ac once,
// and returns the persisted object.
//
// Shared rather than per-file: three untagged suites now drive the real
// Reconcile to assert a DERIVED status field, and a second copy of this setup
// is a second place for the reconciler's dependencies to be wired differently
// from production.
func reconcileClass(t *testing.T, ac *spiceboxv1alpha1.AgentClass, objs ...client.Object) spiceboxv1alpha1.AgentClass {
	t.Helper()
	all := []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
			Data:       map[string][]byte{"api-key": []byte("sk-test")},
		},
		ac,
	}
	all = append(all, objs...)

	c := fakeclient.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(all...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentClass{}).
		Build()
	r := &agentclass.Reconciler{Client: c, APIReader: c}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: ac.Name},
	})
	require.NoError(t, err, "Reconcile must not error")
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: ac.Name}, &got), "Get after reconcile")
	return got
}
