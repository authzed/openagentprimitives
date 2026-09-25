package cloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
)

// discoveryServer serves the minimal /api + /apis discovery documents a
// discovery client reads in ServerGroups(). When gatewayGroup is true the
// gateway.networking.k8s.io group is advertised. This reproduces the real
// cluster behavior a fake client cannot: a missing API group.
func discoveryServer(t *testing.T, gatewayGroup bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIVersions","versions":["v1"]}`))
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		groups := `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`
		if gatewayGroup {
			groups = `{"kind":"APIGroupList","apiVersion":"v1","groups":[` +
				`{"name":"gateway.networking.k8s.io",` +
				`"versions":[{"groupVersion":"gateway.networking.k8s.io/v1","version":"v1"}],` +
				`"preferredVersion":{"groupVersion":"gateway.networking.k8s.io/v1","version":"v1"}}]}`
		}
		_, _ = w.Write([]byte(groups))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestGatewayAPIServed(t *testing.T) {
	cases := []struct {
		name         string
		gatewayGroup bool
		want         bool
	}{
		{name: "group present: served=true", gatewayGroup: true, want: true},
		{name: "group absent: served=false (clean, not a discovery error)", gatewayGroup: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := discoveryServer(t, tc.gatewayGroup)
			got, err := GatewayAPIServed(&rest.Config{Host: srv.URL})
			require.NoError(t, err, "GatewayAPIServed must not error against a reachable apiserver")
			assert.Equal(t, tc.want, got)
		})
	}
}

// gcGVR is the cluster-scoped GatewayClass resource.
var gcGVR = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses"}

// newDynWithGatewayClass returns a fake dynamic client. When existing != "" it
// is pre-seeded with a GatewayClass of that name so the "already present" path
// can be exercised. The scheme lists gatewayclasses so List does not error.
func newDynWithGatewayClass(t *testing.T, existing string) *dynfake.FakeDynamicClient {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	scheme.AddKnownTypeWithName(gcGVR.GroupVersion().WithKind("GatewayClassList"), &unstructured.UnstructuredList{})
	gvrToListKind := map[schema.GroupVersionResource]string{gcGVR: "GatewayClassList"}
	var objs []k8sruntime.Object
	if existing != "" {
		objs = append(objs, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1", "kind": "GatewayClass",
			"metadata": map[string]any{"name": existing},
		}})
	}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, objs...)
}

func TestEnsureEnvoyGatewayController(t *testing.T) {
	t.Run("--gateway-class override: returns it, no install, no class lookup", func(t *testing.T) {
		p := GatewayControllerParams{GatewayClassOverride: "custom-class"}
		res, err := EnsureEnvoyGatewayController(context.Background(), p)
		require.NoError(t, err)
		assert.True(t, res.Proceed)
		assert.Equal(t, "custom-class", res.GatewayClass)
	})

	t.Run("API served + eg class already present: returns eg, no install", func(t *testing.T) {
		orig := servedCheck
		servedCheck = func(*rest.Config) (bool, error) { return true, nil }
		t.Cleanup(func() { servedCheck = orig })

		dyn := newDynWithGatewayClass(t, EnvoyGatewayClass)
		installed := false
		comp := &Component{
			Name:      "Envoy Gateway",
			Present:   func(context.Context) (bool, error) { installed = true; return true, nil },
			Manifests: func() ([][]byte, error) { return nil, nil },
		}
		p := GatewayControllerParams{
			Clients:           Clients{Dynamic: dyn},
			Reporter:          NopReporter{},
			BundledController: comp,
		}
		res, err := EnsureEnvoyGatewayController(context.Background(), p)
		require.NoError(t, err)
		assert.True(t, res.Proceed)
		assert.Equal(t, EnvoyGatewayClass, res.GatewayClass)
		assert.False(t, installed, "must not consult the install Component when a class already exists")
	})

	t.Run("API not served: installs Envoy, creates eg class", func(t *testing.T) {
		orig := servedCheck
		servedCheck = func(*rest.Config) (bool, error) { return false, nil }
		t.Cleanup(func() { servedCheck = orig })

		dyn := newDynWithGatewayClass(t, "")
		comp := &Component{
			Name:      "Envoy Gateway",
			Present:   func(context.Context) (bool, error) { return false, nil }, // not present → installs
			Manifests: func() ([][]byte, error) { return nil, nil },              // empty bundle: apply loop no-ops
		}
		p := GatewayControllerParams{
			Clients:           Clients{Dynamic: dyn},
			Reporter:          NopReporter{},
			AssumeYes:         true,
			BundledController: comp,
		}
		res, err := EnsureEnvoyGatewayController(context.Background(), p)
		require.NoError(t, err)
		assert.True(t, res.Proceed)
		assert.Equal(t, EnvoyGatewayClass, res.GatewayClass)

		got, err := dyn.Resource(gcGVR).Get(context.Background(), EnvoyGatewayClass, metav1.GetOptions{})
		require.NoError(t, err, "eg GatewayClass must have been created")
		assert.Equal(t, EnvoyGatewayClass, got.GetName())
	})

	t.Run("API served, no GatewayClasses: installs Envoy, creates eg class", func(t *testing.T) {
		orig := servedCheck
		servedCheck = func(*rest.Config) (bool, error) { return true, nil }
		t.Cleanup(func() { servedCheck = orig })

		dyn := newDynWithGatewayClass(t, "") // no classes present
		comp := &Component{
			Name:      "Envoy Gateway",
			Present:   func(context.Context) (bool, error) { return false, nil }, // not present → installs
			Manifests: func() ([][]byte, error) { return nil, nil },              // empty bundle: apply loop no-ops
		}
		p := GatewayControllerParams{
			Clients:           Clients{Dynamic: dyn},
			Reporter:          NopReporter{},
			AssumeYes:         true,
			BundledController: comp,
		}
		res, err := EnsureEnvoyGatewayController(context.Background(), p)
		require.NoError(t, err)
		assert.True(t, res.Proceed)
		assert.Equal(t, EnvoyGatewayClass, res.GatewayClass)

		got, err := dyn.Resource(gcGVR).Get(context.Background(), EnvoyGatewayClass, metav1.GetOptions{})
		require.NoError(t, err, "eg GatewayClass must have been created")
		assert.Equal(t, EnvoyGatewayClass, got.GetName())
	})

	t.Run("API served, non-eg class present: returns that class, no install", func(t *testing.T) {
		orig := servedCheck
		servedCheck = func(*rest.Config) (bool, error) { return true, nil }
		t.Cleanup(func() { servedCheck = orig })

		dyn := newDynWithGatewayClass(t, "other-class") // a non-eg class is present
		installCalled := false
		comp := &Component{
			Name:      "Envoy Gateway",
			Present:   func(context.Context) (bool, error) { installCalled = true; return true, nil },
			Manifests: func() ([][]byte, error) { return nil, nil },
		}
		p := GatewayControllerParams{
			Clients:           Clients{Dynamic: dyn},
			Reporter:          NopReporter{},
			AssumeYes:         true,
			BundledController: comp,
		}
		res, err := EnsureEnvoyGatewayController(context.Background(), p)
		require.NoError(t, err)
		assert.True(t, res.Proceed)
		assert.Equal(t, "other-class", res.GatewayClass, "must return the existing non-eg class")
		assert.False(t, installCalled, "must not consult install Component when a class already exists")
	})
}
