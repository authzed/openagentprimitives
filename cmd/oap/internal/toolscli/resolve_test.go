package toolscli_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/toolscli"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	// Side-effect imports register the kinds.
	_ "github.com/authzed/openagentprimitives/pkg/tools/kinds/mcp"
	_ "github.com/authzed/openagentprimitives/pkg/tools/kinds/sandbox"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, fn := range []func(*runtime.Scheme) error{corev1.AddToScheme, spiceboxv1alpha1.AddToScheme} {
		require.NoError(t, fn(s), "AddToScheme")
	}
	return s
}

func TestResolve_NotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	_, err := toolscli.Resolve(context.Background(), c, "ns", "missing")
	assert.ErrorIs(t, err, toolscli.ErrNotFound, "Resolve for an absent name returns ErrNotFound")
}

func TestResolve_SingleMatch(t *testing.T) {
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "ns"},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(srv).Build()
	m, err := toolscli.Resolve(context.Background(), c, "ns", "linear")
	require.NoError(t, err, "Resolve must succeed")
	assert.Equal(t, "MCPServer", m.Kind.Name())
	assert.Equal(t, "linear", m.Obj.GetName())
}

func TestResolve_AmbiguousAcrossKinds(t *testing.T) {
	// Seed both an MCPServer (namespaced) and a SpiceboxToolspec
	// (cluster-scoped) named "linear". Resolve looks up the cluster-scoped
	// kind at "" regardless of the requested ns, producing an ambiguity
	// when the same name exists in both kinds.
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "ns"},
	}
	tsp := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "linear"},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(mcp, tsp).Build()

	_, err := toolscli.Resolve(context.Background(), c, "ns", "linear")
	require.Error(t, err, "expected ambiguous error")
	var amb *toolscli.AmbiguousError
	require.True(t, errors.As(err, &amb), "err = %v; want *AmbiguousError", err)
	assert.Len(t, amb.Kinds, 2, "ambiguous err should list both kinds")
}
