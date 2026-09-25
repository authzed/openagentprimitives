package certmanager

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

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// certifcateGVR mirrors the cert-manager Certificate GVR used by
// cloud.ApplyWebdCertificate for building a fake dynamic client in tests.
var certGVR = schema.GroupVersionResource{
	Group: "cert-manager.io", Version: "v1", Resource: "certificates",
}

func newFakeClients(objs ...runtime.Object) cloud.Clients {
	scheme := runtime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			clusterIssuerGVR: "ClusterIssuerList",
			certGVR:          "CertificateList",
		}, objs...)
	return cloud.Clients{Dynamic: dyn}
}

// TestStrategyName verifies Strategy satisfies cloud.TLSStrategy and returns
// the expected stable identifier.
func TestStrategyName(t *testing.T) {
	var s cloud.TLSStrategy = Strategy{}
	assert.Equal(t, "cert-manager", s.Name())
}

// TestPrepare_MissingACMEEmail fails closed when --acme-email is absent (no
// existing --tls-issuer) because cert-manager can't register an ACME account
// without one.
func TestPrepare_MissingACMEEmail(t *testing.T) {
	cl := newFakeClients()
	_, _, proceed, err := Strategy{}.Prepare(context.Background(), cloud.PrepareParams{
		Clients:         cl,
		Reporter:        cloud.NopReporter{},
		TrustedHostname: "webd.example.com",
		// ACMEEmail deliberately absent; TLSIssuer also absent
	})
	require.Error(t, err, "must fail closed when ACMEEmail is missing")
	assert.False(t, proceed)
	assert.Contains(t, err.Error(), "--acme-email")
}

// TestPrepare_ExistingTLSIssuer skips the cert-manager component ensure and
// issuer-creation path; returns a valid GatewayTLS with proceed=true even
// though no cert-manager CRD is present.
func TestPrepare_ExistingTLSIssuer(t *testing.T) {
	cl := newFakeClients()
	gw, dns, proceed, err := Strategy{}.Prepare(context.Background(), cloud.PrepareParams{
		Clients:         cl,
		Reporter:        cloud.NopReporter{},
		TrustedHostname: "webd.example.com",
		TLSIssuer:       "my-existing-issuer",
	})
	require.NoError(t, err)
	assert.True(t, proceed)
	assert.Empty(t, dns, "cert-manager HTTP-01 needs no extra DNS records")
	assert.True(t, gw.Listener.UsesListenerSecrets)
	require.NotNil(t, gw.Listener.SecretFor)
	assert.Equal(t, cloud.WebdTrustedCertSecretName, gw.Listener.SecretFor("webd.example.com"))
}

// TestPrepare_SandboxHostMapsToSandboxSecret verifies the SecretFor closure maps
// the sandbox host to the sandbox Secret and any other non-empty host to the
// trusted Secret.
func TestPrepare_SandboxHostMapsToSandboxSecret(t *testing.T) {
	cl := newFakeClients()
	gw, _, _, err := Strategy{}.Prepare(context.Background(), cloud.PrepareParams{
		Clients:         cl,
		Reporter:        cloud.NopReporter{},
		TrustedHostname: "webd.example.com",
		SandboxHostname: "sandbox.example.com",
		TLSIssuer:       "existing-issuer",
	})
	require.NoError(t, err)
	require.NotNil(t, gw.Listener.SecretFor)
	assert.Equal(t, cloud.WebdSandboxCertSecretName, gw.Listener.SecretFor("sandbox.example.com"),
		"sandbox host must map to sandbox Secret")
	assert.Equal(t, cloud.WebdTrustedCertSecretName, gw.Listener.SecretFor("webd.example.com"),
		"trusted host must map to trusted Secret")
	assert.Equal(t, "", gw.Listener.SecretFor(""),
		"empty host returns empty string")
}

// TestComplete_AppliesCertificates verifies that Complete creates cert-manager
// Certificate objects for the trusted and sandbox hostnames under the resolved
// issuer.
func TestComplete_AppliesCertificates(t *testing.T) {
	cl := newFakeClients()
	err := Strategy{}.Complete(context.Background(), cloud.CompleteParams{
		Clients:         cl,
		Reporter:        cloud.NopReporter{},
		TrustedHostname: "webd.example.com",
		SandboxHostname: "sandbox.example.com",
		TLSIssuer:       "my-issuer",
	})
	require.NoError(t, err)

	// trusted Certificate must exist
	trusted, err := cl.Dynamic.Resource(certGVR).Namespace(cloud.WebdServiceNamespace).
		Get(context.Background(), cloud.WebdTrustedCertSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "true", trusted.GetAnnotations()["cert-manager.io/issue-temporary-certificate"],
		"issue-temporary-certificate annotation must be set to break program-vs-issue deadlock")
	issuer, _, _ := unstructured.NestedString(trusted.Object, "spec", "issuerRef", "name")
	assert.Equal(t, "my-issuer", issuer)

	// sandbox Certificate must also exist
	sandbox, err := cl.Dynamic.Resource(certGVR).Namespace(cloud.WebdServiceNamespace).
		Get(context.Background(), cloud.WebdSandboxCertSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	sandboxIssuer, _, _ := unstructured.NestedString(sandbox.Object, "spec", "issuerRef", "name")
	assert.Equal(t, "my-issuer", sandboxIssuer)
}

// TestComplete_NoSandboxHostname verifies Complete only creates the trusted
// Certificate when no sandbox hostname is given.
func TestComplete_NoSandboxHostname(t *testing.T) {
	cl := newFakeClients()
	err := Strategy{}.Complete(context.Background(), cloud.CompleteParams{
		Clients:         cl,
		Reporter:        cloud.NopReporter{},
		TrustedHostname: "webd.example.com",
		TLSIssuer:       "my-issuer",
	})
	require.NoError(t, err)

	// trusted Certificate must exist
	_, err = cl.Dynamic.Resource(certGVR).Namespace(cloud.WebdServiceNamespace).
		Get(context.Background(), cloud.WebdTrustedCertSecretName, metav1.GetOptions{})
	require.NoError(t, err, "trusted Certificate must be created")

	// sandbox Certificate must NOT exist
	_, err = cl.Dynamic.Resource(certGVR).Namespace(cloud.WebdServiceNamespace).
		Get(context.Background(), cloud.WebdSandboxCertSecretName, metav1.GetOptions{})
	assert.Error(t, err, "no sandbox Certificate should be created when SandboxHostname is empty")
}

// TestComplete_DefaultIssuerWhenTLSIssuerAbsent verifies that when TLSIssuer
// is empty, Complete uses cloud.WebdLetsEncryptIssuerName (the LE issuer oap
// creates in Prepare).
func TestComplete_DefaultIssuerWhenTLSIssuerAbsent(t *testing.T) {
	cl := newFakeClients()
	err := Strategy{}.Complete(context.Background(), cloud.CompleteParams{
		Clients:         cl,
		Reporter:        cloud.NopReporter{},
		TrustedHostname: "webd.example.com",
		// TLSIssuer absent: should default to WebdLetsEncryptIssuerName
	})
	require.NoError(t, err)

	got, err := cl.Dynamic.Resource(certGVR).Namespace(cloud.WebdServiceNamespace).
		Get(context.Background(), cloud.WebdTrustedCertSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	issuer, _, _ := unstructured.NestedString(got.Object, "spec", "issuerRef", "name")
	assert.Equal(t, cloud.WebdLetsEncryptIssuerName, issuer,
		"empty TLSIssuer must fall back to the oap-created LE issuer")
}

// TestCertManagerComponent_ManifestLoads verifies the embedded cert-manager YAML
// can be read from the embedded FS without error.
func TestCertManagerComponent_ManifestLoads(t *testing.T) {
	cl := newFakeClients()
	comp := CertManagerComponent(cl)
	groups, err := comp.Manifests()
	require.NoError(t, err)
	require.Len(t, groups, 1, "cert-manager manifest must be one multi-doc group")
	assert.NotEmpty(t, groups[0], "manifest bytes must be non-empty")
}
