package cloud

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// gatewayScheme returns a runtime.Scheme with the Gateway API types installed.
// Only used by gateway_test.go so it doesn't pollute the package-level state.
func gatewayScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(s))
	return s
}

// newGWClients builds a cloud.Clients with a controller-runtime fake client
// that has the Gateway API scheme registered.
func newGWClients(t *testing.T, objs ...client.Object) Clients {
	t.Helper()
	s := gatewayScheme(t)
	return Clients{
		Ctrl: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
	}
}

// secretBackedGWTLS is the cert-manager-shaped GatewayTLS (each HTTPS listener
// references a per-hostname Secret) used by the Gateway tests that exercise the
// default flow.
func secretBackedGWTLS(sandboxHost string) GatewayTLS {
	return GatewayTLS{
		Listener: ListenerTLS{
			UsesListenerSecrets: true,
			SecretFor: func(host string) string {
				if host == sandboxHost && sandboxHost != "" {
					return WebdSandboxCertSecretName
				}
				return WebdTrustedCertSecretName
			},
		},
	}
}

func TestApplyWebdGateway_TwoOriginsPlusACME(t *testing.T) {
	cl := newGWClients(t)
	require.NoError(t, ApplyWebdGateway(context.Background(), cl,
		"envoy-gateway", "webd.example.com", "artifacts.example.com", secretBackedGWTLS("artifacts.example.com")))

	var gw gatewayv1.Gateway
	require.NoError(t, cl.Ctrl.Get(context.Background(), client.ObjectKey{
		Namespace: WebdServiceNamespace, Name: WebdGatewayName}, &gw))

	assert.Equal(t, gatewayv1.ObjectName("envoy-gateway"), gw.Spec.GatewayClassName)
	var https, http int
	for _, l := range gw.Spec.Listeners {
		switch l.Protocol {
		case gatewayv1.HTTPSProtocolType:
			https++
			require.NotNil(t, l.TLS)
			require.Len(t, l.TLS.CertificateRefs, 1)
		case gatewayv1.HTTPProtocolType:
			http++
		}
	}
	assert.Equal(t, 2, https)
	assert.Equal(t, 1, http)
	// oap issues the listener certs via explicit Certificates, not the
	// gateway-shim, so no cluster-issuer annotation is stamped on the Gateway.
	assert.Empty(t, gw.Annotations["cert-manager.io/cluster-issuer"])
}

// TestApplyWebdGateway_GoogleManagedTLS exercises the google-managed shape: no
// per-listener certificateRefs (the cert is attached via the certmap annotation
// the strategy supplies).
func TestApplyWebdGateway_GoogleManagedTLS(t *testing.T) {
	cl := newGWClients(t)
	gwTLS := GatewayTLS{
		Listener:    ListenerTLS{UsesListenerSecrets: false},
		Annotations: map[string]string{"networking.gke.io/certmap": "spicebox-webd"},
	}
	require.NoError(t, ApplyWebdGateway(context.Background(), cl,
		"gke-l7-global-external-managed", "webd.example.com", "artifacts.example.com", gwTLS))

	var gw gatewayv1.Gateway
	require.NoError(t, cl.Ctrl.Get(context.Background(), client.ObjectKey{
		Namespace: WebdServiceNamespace, Name: WebdGatewayName}, &gw))

	for _, l := range gw.Spec.Listeners {
		if l.Protocol == gatewayv1.HTTPSProtocolType {
			assert.Nil(t, l.TLS, "google-managed HTTPS listeners carry no certificateRefs")
		}
	}
	assert.Equal(t, "spicebox-webd", gw.Annotations["networking.gke.io/certmap"])
}

func TestApplyWebdGateway_Idempotent(t *testing.T) {
	cl := newGWClients(t)
	require.NoError(t, ApplyWebdGateway(context.Background(), cl, "envoy-gateway", "w.example.com", "a.example.com", secretBackedGWTLS("a.example.com")))
	require.NoError(t, ApplyWebdGateway(context.Background(), cl, "envoy-gateway", "w.example.com", "a.example.com", secretBackedGWTLS("a.example.com")))
}

func TestApplyLetsEncryptClusterIssuer(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{clusterIssuerGVR: "ClusterIssuerList"})
	cl := Clients{Dynamic: dyn}

	require.NoError(t, ApplyLetsEncryptClusterIssuer(context.Background(), cl,
		WebdLetsEncryptIssuerName, "ops@example.com", WebdGatewayName, WebdServiceNamespace))

	got, err := dyn.Resource(clusterIssuerGVR).Get(context.Background(), WebdLetsEncryptIssuerName, metav1.GetOptions{})
	require.NoError(t, err)

	email, found, err := unstructured.NestedString(got.Object, "spec", "acme", "email")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "ops@example.com", email)

	server, _, _ := unstructured.NestedString(got.Object, "spec", "acme", "server")
	assert.Contains(t, server, "acme-v02.api.letsencrypt.org")

	solvers, found, err := unstructured.NestedSlice(got.Object, "spec", "acme", "solvers")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, solvers, 1)
	// solver must be http01 / gatewayHTTPRoute
	solver0 := solvers[0].(map[string]any)
	_, hasHTTP01 := solver0["http01"].(map[string]any)
	assert.True(t, hasHTTP01, "solver should be http01")
}

func TestApplyLetsEncryptClusterIssuer_Idempotent(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{clusterIssuerGVR: "ClusterIssuerList"})
	cl := Clients{Dynamic: dyn}
	require.NoError(t, ApplyLetsEncryptClusterIssuer(context.Background(), cl, WebdLetsEncryptIssuerName, "ops@example.com", WebdGatewayName, WebdServiceNamespace))
	require.NoError(t, ApplyLetsEncryptClusterIssuer(context.Background(), cl, WebdLetsEncryptIssuerName, "ops@example.com", WebdGatewayName, WebdServiceNamespace))
}

func TestApplyWebdCertificate(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{certificateGVR: "CertificateList"})
	cl := Clients{Dynamic: dyn}

	require.NoError(t, ApplyWebdCertificate(context.Background(), cl, WebdServiceNamespace,
		WebdTrustedCertSecretName, "webd.example.com", WebdLetsEncryptIssuerName))

	got, err := dyn.Resource(certificateGVR).Namespace(WebdServiceNamespace).
		Get(context.Background(), WebdTrustedCertSecretName, metav1.GetOptions{})
	require.NoError(t, err)

	// The temp-cert annotation is what breaks the GKE program-vs-issue deadlock.
	assert.Equal(t, "true", got.GetAnnotations()["cert-manager.io/issue-temporary-certificate"])

	secretName, _, _ := unstructured.NestedString(got.Object, "spec", "secretName")
	assert.Equal(t, WebdTrustedCertSecretName, secretName)
	dnsNames, _, _ := unstructured.NestedStringSlice(got.Object, "spec", "dnsNames")
	assert.Equal(t, []string{"webd.example.com"}, dnsNames)
	issuer, _, _ := unstructured.NestedString(got.Object, "spec", "issuerRef", "name")
	assert.Equal(t, WebdLetsEncryptIssuerName, issuer)
	kind, _, _ := unstructured.NestedString(got.Object, "spec", "issuerRef", "kind")
	assert.Equal(t, "ClusterIssuer", kind)
}

func TestApplyWebdCertificate_Idempotent(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{certificateGVR: "CertificateList"})
	cl := Clients{Dynamic: dyn}
	require.NoError(t, ApplyWebdCertificate(context.Background(), cl, WebdServiceNamespace, WebdTrustedCertSecretName, "webd.example.com", WebdLetsEncryptIssuerName))
	require.NoError(t, ApplyWebdCertificate(context.Background(), cl, WebdServiceNamespace, WebdTrustedCertSecretName, "webd.example.com", WebdLetsEncryptIssuerName))
}
