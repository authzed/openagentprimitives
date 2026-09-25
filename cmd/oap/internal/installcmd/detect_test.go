package installcmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// detectClusterIssuerGVR mirrors the unexported GVR
// cloud.DetectLetsEncryptEmail reads (pkg/platform/cloud/gateway.go). It
// isn't exported for a single out-of-package test to reach, so the fixture
// reproduces the GVR rather than depending on cloud's internals.
var detectClusterIssuerGVR = schema.GroupVersionResource{
	Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers",
}

func issuerObj(email string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "ClusterIssuer",
		"metadata":   map[string]any{"name": cloud.WebdLetsEncryptIssuerName},
		"spec":       map[string]any{"acme": map[string]any{"email": email}},
	}}
}

func fakeDynWithIssuer(t *testing.T, email string) *dynfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{detectClusterIssuerGVR: "ClusterIssuerList"}, issuerObj(email))
}

func fakeDynNoIssuer(t *testing.T) *dynfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{detectClusterIssuerGVR: "ClusterIssuerList"})
}

func fakeCtrlWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(objs...).Build()
}

// fakeTypedWithMarker seeds the workspace marker ConfigMap
// (readWorkspaceMarker, workspace.go:262) that detectSettings reads through
// the typed clientset, not the controller-runtime client.
func fakeTypedWithMarker(t *testing.T, class string) kubernetes.Interface {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceMarkerName, Namespace: workspaceMarkerNamespace},
		Data:       map[string]string{workspaceMarkerKey: class},
	}
	return k8sfake.NewSimpleClientset(cm)
}

func fakeTypedNoMarker(t *testing.T) kubernetes.Interface {
	t.Helper()
	return k8sfake.NewSimpleClientset()
}

func idpCR(kind string) *spiceboxv1alpha1.ClusterIdentityProvider {
	return &spiceboxv1alpha1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterIdentityProviderName},
		Spec: spiceboxv1alpha1.ClusterIdentityProviderSpec{
			Kind:     kind,
			ClientID: "demo-idp-client",
			ClientSecretRef: spiceboxv1alpha1.ClusterSecretKeyRef{
				Namespace: "agentprimitives-system",
				Name:      "demo-idp-secret",
				Key:       "clientSecret",
			},
		},
	}
}

func monitoringChannel(name, kind string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agentprimitives-system"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Role: spiceboxv1alpha1.ChannelRoleMonitoring, Kind: kind},
	}
}

// webdRoute builds an installed webd HTTPRoute carrying host as its first
// spec.hostname — the durable trace detectSettings reads the external-access
// hostnames from. The controller-runtime fake client uses kube.Scheme, which
// installs gateway-api, so no extra scheme registration is needed.
func webdRoute(name, host string) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cloud.WebdServiceNamespace},
		Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(host)}},
	}
}

func TestDetectSettings(t *testing.T) {
	t.Run("existing install: fields populated, Existing=true", func(t *testing.T) {
		d := detectDeps{
			Dyn:       fakeDynWithIssuer(t, "demo@example.test"),
			Ctrl:      fakeCtrlWith(t, idpCR("google"), monitoringChannel("demo-mon", "slack")),
			Typed:     fakeTypedWithMarker(t, "ap-workspace-rwx"),
			Namespace: "agentprimitives-system",
		}
		got, err := detectSettings(context.Background(), d)
		require.NoError(t, err)
		assert.True(t, got.Existing)
		assert.Equal(t, "demo@example.test", got.ACMEEmail)
		assert.Equal(t, "ap-workspace-rwx", got.WorkspaceClass)
		assert.Equal(t, "google", got.IdPKind)
		assert.Equal(t, "demo-mon", got.MonitoringChannel)
		// No webd HTTPRoutes in this fixture, so the hostnames stay empty.
		assert.Empty(t, got.TrustedHostname)
		assert.Empty(t, got.SandboxHostname)
		assert.False(t, got.ExternalSpiceDB)
	})

	t.Run("existing webd routes: hostnames detected, Existing=true", func(t *testing.T) {
		d := detectDeps{
			Dyn: fakeDynNoIssuer(t),
			Ctrl: fakeCtrlWith(t,
				webdRoute(webdTrustedRouteName, "webd.example.com"),
				webdRoute(webdSandboxRouteName, "sandbox.example.com")),
			Typed:     fakeTypedNoMarker(t),
			Namespace: "agentprimitives-system",
		}
		got, err := detectSettings(context.Background(), d)
		require.NoError(t, err)
		assert.Equal(t, "webd.example.com", got.TrustedHostname)
		assert.Equal(t, "sandbox.example.com", got.SandboxHostname)
		// A detected trusted hostname alone marks the install existing, even with
		// no issuer, marker, IdP, or monitoring channel present.
		assert.True(t, got.Existing, "a detected trusted hostname marks the install existing")
	})

	t.Run("fresh cluster: zero values, Existing=false", func(t *testing.T) {
		d := detectDeps{
			Dyn:       fakeDynNoIssuer(t),
			Ctrl:      fakeCtrlWith(t),
			Typed:     fakeTypedNoMarker(t),
			Namespace: "agentprimitives-system",
		}
		got, err := detectSettings(context.Background(), d)
		require.NoError(t, err)
		assert.False(t, got.Existing)
		assert.Empty(t, got.ACMEEmail)
		assert.Empty(t, got.WorkspaceClass)
		assert.Empty(t, got.IdPKind)
		assert.Empty(t, got.MonitoringChannel)
	})

	t.Run("external SpiceDB config: ExternalSpiceDB+endpoint detected, Existing=true", func(t *testing.T) {
		extCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "spicebox-spicedb-config", Namespace: cloud.WebdServiceNamespace},
			Data:       map[string]string{"endpoint": "spicedb.example.com:443"},
		}
		d := detectDeps{
			Dyn:       fakeDynNoIssuer(t),
			Ctrl:      fakeCtrlWith(t),
			Typed:     k8sfake.NewSimpleClientset(extCM),
			Namespace: "agentprimitives-system",
		}
		got, err := detectSettings(context.Background(), d)
		require.NoError(t, err)
		assert.True(t, got.ExternalSpiceDB, "an off-cluster SpiceDB endpoint marks the backend external")
		assert.Equal(t, "spicedb.example.com:443", got.ExternalSpiceDBEndpoint)
		assert.True(t, got.Existing, "an external SpiceDB config is a durable install trace on its own")
	})

	t.Run("in-cluster SpiceDB config: ExternalSpiceDB=false", func(t *testing.T) {
		inCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "spicebox-spicedb-config", Namespace: cloud.WebdServiceNamespace},
			Data:       map[string]string{"endpoint": "spicebox-spicedb.agentprimitives-system.svc:50051"},
		}
		d := detectDeps{
			Dyn:       fakeDynNoIssuer(t),
			Ctrl:      fakeCtrlWith(t),
			Typed:     k8sfake.NewSimpleClientset(inCM),
			Namespace: "agentprimitives-system",
		}
		got, err := detectSettings(context.Background(), d)
		require.NoError(t, err)
		assert.False(t, got.ExternalSpiceDB, "the bundled in-cluster Service endpoint is not external")
		assert.Empty(t, got.ExternalSpiceDBEndpoint)
		assert.False(t, got.Existing)
	})

	t.Run("unexpected API error on the IdP read propagates, wrapped", func(t *testing.T) {
		wantErr := errors.New("apiserver unreachable")
		ctrl := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apierrors.NewInternalError(wantErr)
			},
		}).Build()
		d := detectDeps{
			Dyn:       fakeDynNoIssuer(t),
			Ctrl:      ctrl,
			Typed:     fakeTypedNoMarker(t),
			Namespace: "agentprimitives-system",
		}
		_, err := detectSettings(context.Background(), d)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "detect identity provider")
	})

	t.Run("missing CRDs (no Gateway API / not-yet-installed cluster): zero values, no error", func(t *testing.T) {
		// A fresh cluster (or an existing --local install with no Gateway API
		// CRDs) answers every Get/List detectSettings issues through d.Ctrl with
		// *meta.NoKindMatchError, not apierrors.IsNotFound — the controller-runtime
		// client resolves GVK->GVR via its RESTMapper before ever reaching the
		// apiserver, so a missing CRD's group is never registered in that mapper.
		// This must be tolerated exactly like NotFound: leave the field empty,
		// don't crash the wizard before its first screen.
		noMatch := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute"}}
		ctrl := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return noMatch
			},
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return noMatch
			},
		}).Build()
		d := detectDeps{
			Dyn:       fakeDynNoIssuer(t),
			Ctrl:      ctrl,
			Typed:     fakeTypedNoMarker(t),
			Namespace: "agentprimitives-system",
		}
		got, err := detectSettings(context.Background(), d)
		require.NoError(t, err)
		assert.Equal(t, DetectedSettings{}, got, "a missing-CRD cluster must detect the zero settings, not crash")
	})
}
