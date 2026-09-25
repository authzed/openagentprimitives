package hook_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/platform/capacityfit/hook"
)

// fakeSpiceboxClassCR builds an unstructured SpiceboxClass declaring memory —
// the minimal shape pkg/platform/capacityfit.Questions needs. A made-up fixture name.
func fakeSpiceboxClassCR(name, mem string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "SpiceboxClass",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"resources": map[string]any{"memory": mem}},
	}}
}

func fakeNode(name, mem string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
		},
	}
}

func TestNew(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()

	t.Run("nil typed -> skip with notice, no panic", func(t *testing.T) {
		h := hook.New(ctx, nil, fake.NewClientBuilder().WithScheme(scheme).Build())
		qs, notices, err := h(ctx, []*unstructured.Unstructured{fakeSpiceboxClassCR("demo-class", "4Gi")})

		require.NoError(t, err)
		assert.Empty(t, qs)
		require.Len(t, notices, 1)
		assert.Contains(t, notices[0], "no cluster clientset configured")
	})

	t.Run("nil ctrl -> skip with notice, no panic", func(t *testing.T) {
		h := hook.New(ctx, k8sfake.NewSimpleClientset(), nil)
		qs, notices, err := h(ctx, []*unstructured.Unstructured{fakeSpiceboxClassCR("demo-class", "4Gi")})

		require.NoError(t, err)
		assert.Empty(t, qs)
		require.Len(t, notices, 1)
		assert.Contains(t, notices[0], "no cluster clientset configured")
	})

	// M5 regression: this package deliberately does NOT blank-import any
	// pkg/platform/cloud/{local,gke,eks,aks} — that is a BINARY's job
	// (cmd/oap/cloudimports.go, internal/cmd/operator/cloudimports.go), never a
	// library's. So in THIS test binary cloud.Default() finds no KeyDefault
	// kind registered and errors, exactly the "a binary forgot to wire one"
	// scenario — hook.go folds that error into its own skip notice rather than
	// guarding a nil Strategy.
	t.Run("no cloud.Strategy registered anywhere in this test binary -> skip with notice, no panic", func(t *testing.T) {
		h := hook.New(ctx, k8sfake.NewSimpleClientset(fakeNode("n1", "8Gi")), fake.NewClientBuilder().WithScheme(scheme).Build())
		qs, notices, err := h(ctx, []*unstructured.Unstructured{fakeSpiceboxClassCR("demo-class", "4Gi")})

		require.NoError(t, err)
		assert.Empty(t, qs)
		require.Len(t, notices, 1)
		assert.Contains(t, notices[0], "cluster kind registered")
	})
}
