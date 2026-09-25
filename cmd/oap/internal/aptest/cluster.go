package aptest

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Scheme is the scheme every oap command decodes against: the AP CRs plus the
// core types (Secrets, ConfigMaps) the commands read alongside them.
func Scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// NewFakeBundle builds a kube.Bundle backed by a fake controller client (for
// the pre-apply lookups) and a fake dynamic client (for the applies), so a test
// can assert on the objects a run actually created rather than on its stdout.
//
// listKinds names the list kind for each GVR the run applies through the
// dynamic client; the fake dynamic client cannot synthesize them.
func NewFakeBundle(t *testing.T, listKinds map[schema.GroupVersionResource]string, objs ...client.Object) (*kube.Bundle, *dynfake.FakeDynamicClient) {
	t.Helper()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	return &kube.Bundle{
		Controller: fake.NewClientBuilder().WithScheme(Scheme(t)).WithObjects(objs...).Build(),
		Dynamic:    dyn,
		Namespace:  "default",
	}, dyn
}

// NewBundle is NewFakeBundle for the commands that only read through the
// controller client — no dynamic applies, so no list kinds to declare.
func NewBundle(t *testing.T, objs ...client.Object) *kube.Bundle {
	t.Helper()
	b, _ := NewFakeBundle(t, map[schema.GroupVersionResource]string{}, objs...)
	return b
}

// ClientBuilder returns a fake.ClientBuilder over Scheme, for the tests that
// need to keep configuring it (interceptors, status subresources) before
// building.
func ClientBuilder(t *testing.T) *fake.ClientBuilder {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(Scheme(t))
}

// IdentityClientBuilder is ClientBuilder with AgentIdentity's status treated
// as a subresource, which the identity setup flows write through.
func IdentityClientBuilder(t *testing.T) *fake.ClientBuilder {
	t.Helper()
	return ClientBuilder(t).WithStatusSubresource(&spiceboxv1alpha1.AgentIdentity{})
}
