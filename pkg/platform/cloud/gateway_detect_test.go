package cloud

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

func issuerObj(email string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "ClusterIssuer",
		"metadata":   map[string]any{"name": WebdLetsEncryptIssuerName},
		"spec":       map[string]any{"acme": map[string]any{"email": email}},
	}}
}

func newDetectDyn(t *testing.T, objs ...runtime.Object) *dynfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{clusterIssuerGVR: "ClusterIssuerList"}, objs...)
}

func TestDetectLetsEncryptEmail(t *testing.T) {
	cases := []struct {
		name string
		objs []runtime.Object
		want string
	}{
		{name: "issuer present: returns spec.acme.email", objs: []runtime.Object{issuerObj("demo@example.test")}, want: "demo@example.test"},
		{name: "no issuer (fresh cluster): empty, no error", objs: nil, want: ""},
		{name: "issuer present, email unset: empty, no error", objs: []runtime.Object{issuerObj("")}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectLetsEncryptEmail(context.Background(), Clients{Dynamic: newDetectDyn(t, tc.objs...)})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
