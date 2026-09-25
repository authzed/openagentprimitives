package settingsui

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

// newInstallInfoBundle builds a *kube.Bundle whose Typed is a client-go fake
// clientset seeded with objs. Only Typed is set — the install-info handler
// touches nothing else on the Bundle.
func newInstallInfoBundle(objs ...runtime.Object) *kube.Bundle {
	return &kube.Bundle{Typed: k8sfake.NewSimpleClientset(objs...)}
}

// operatorDeployment returns the seed operator Deployment: a mix of
// allowlisted plain-Value env vars (one per operatorEnvAllowlist entry
// except SECRET_GUARD_MODE), SECRET_GUARD_MODE as a ValueFrom (proving an
// ALLOWLISTED name is still excluded when it isn't a plain Value), and two
// env vars that must never appear in the response at all: POSTGRES_URI (a
// plain Value carrying a credential, not on the allowlist) and GITHUB_TOKEN
// (a ValueFrom, also not on the allowlist).
func operatorDeployment(readyReplicas, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: apcmd.OperatorDeployment, Namespace: apcmd.SystemNamespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "operator",
						Image: "ghcr.io/example/spicebox-operator:dev",
						Env: []corev1.EnvVar{
							{Name: "MEMORY_BACKEND", Value: "sqlite"},
							{Name: "MEMORY_READ_SOURCE", Value: "primary"},
							{Name: "KG_INGESTION_STRATEGY", Value: "every_turn"},
							{Name: "AP_CLUSTER_KIND", Value: "desktop"},
							{Name: "WATCH_NAMESPACES", Value: "agentprimitives-system"},
							{Name: "ARTIFACT_STORE_URL", Value: "file:///data/artifacts"},
							{Name: "SECRET_GUARD_MODE", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "guard-mode-secret"},
									Key:                  "mode",
								},
							}},
							{Name: "POSTGRES_URI", Value: "postgres://appuser:s3cr3t-password@postgres.agentprimitives-system:5432/ap"},
							{Name: "GITHUB_TOKEN", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "github-token-secret"},
									Key:                  "token",
								},
							}},
						},
					}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: readyReplicas, Replicas: replicas},
	}
}

func channelsdDeployment(readyReplicas, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "channelsd", Namespace: apcmd.SystemNamespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "channelsd",
						Image: "ghcr.io/example/agentprimitives-channelsd:dev",
					}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: readyReplicas, Replicas: replicas},
	}
}

func readyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.29.4"},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func TestInstallInfo_HappyProjection(t *testing.T) {
	b := newInstallInfoBundle(
		operatorDeployment(1, 1),
		channelsdDeployment(2, 3),
		readyNode("desktop-node-1"),
	)
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }, AppVersion: "v0.1.0-test"})
	c := authedClient(t, s)

	resp := doJSON(t, c, http.MethodGet, "http://"+s.Addr()+"/api/cluster/install-info", nil)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	var got installInfoResponse
	require.NoError(t, json.Unmarshal(body, &got))

	assert.False(t, got.ClusterDown)
	assert.Equal(t, "desktop", got.ClusterKind)
	assert.Equal(t, "v0.1.0-test", got.AppVersion, "echoed verbatim from Deps.AppVersion")

	require.Len(t, got.Components, 2, "sorted by name: channelsd, spicebox-operator")
	assert.Equal(t, componentInfo{Name: "channelsd", Image: "ghcr.io/example/agentprimitives-channelsd:dev", Ready: "2/3"}, got.Components[0])
	assert.Equal(t, componentInfo{Name: apcmd.OperatorDeployment, Image: "ghcr.io/example/spicebox-operator:dev", Ready: "1/1"}, got.Components[1])

	assert.ElementsMatch(t, []envEntry{
		{Name: "MEMORY_BACKEND", Value: "sqlite"},
		{Name: "MEMORY_READ_SOURCE", Value: "primary"},
		{Name: "KG_INGESTION_STRATEGY", Value: "every_turn"},
		{Name: "AP_CLUSTER_KIND", Value: "desktop"},
		{Name: "WATCH_NAMESPACES", Value: "agentprimitives-system"},
		{Name: "ARTIFACT_STORE_URL", Value: "file:///data/artifacts"},
	}, got.OperatorEnv, "exactly the allowlisted plain-Value entries — no POSTGRES_URI, no ValueFrom entries")

	// Env entries serialize with camelCase keys like every other field in
	// this package's wire shapes — locked at the raw-body level because the
	// parsed-struct assertions above would pass regardless of tag casing.
	assert.Contains(t, string(body), `{"name":"MEMORY_BACKEND","value":"sqlite"}`)
	assert.NotContains(t, string(body), `"Name":`)

	require.NotNil(t, got.Node)
	assert.Equal(t, "desktop-node-1", got.Node.Name)
	assert.Equal(t, "v1.29.4", got.Node.KubeletVersion)
	assert.True(t, got.Node.Ready)
}

func TestInstallInfo_ClusterDown_ReturnsDownFlag200(t *testing.T) {
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return nil, assert.AnError }})
	c := authedClient(t, s)

	resp := doJSON(t, c, http.MethodGet, "http://"+s.Addr()+"/api/cluster/install-info", nil)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got installInfoResponse
	require.NoError(t, json.Unmarshal(body, &got))
	assert.True(t, got.ClusterDown)
	assert.Empty(t, got.Components)
	assert.Empty(t, got.OperatorEnv)
	assert.Nil(t, got.Node)

	// Components/OperatorEnv must be "[]" on the wire, never "null" — a
	// frontend that unconditionally .map()s these arrays must not have to
	// special-case the down state.
	assert.Contains(t, string(body), `"components":[]`)
	assert.Contains(t, string(body), `"operatorEnv":[]`)
}

func TestInstallInfo_Redaction_NeverLeaksSecretsOrValueFrom(t *testing.T) {
	b := newInstallInfoBundle(operatorDeployment(1, 1), readyNode("n1"))
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
	c := authedClient(t, s)

	resp := doJSON(t, c, http.MethodGet, "http://"+s.Addr()+"/api/cluster/install-info", nil)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	raw := string(body)
	for _, leak := range []string{
		"POSTGRES_URI",
		"s3cr3t-password",
		"postgres://",
		"GITHUB_TOKEN",
		"github-token-secret",
		"SECRET_GUARD_MODE",
		"guard-mode-secret",
	} {
		assert.NotContains(t, raw, leak, "must never appear in the raw response body")
	}
}

func TestInstallInfo_OperatorAbsent_OmitsEnvAndClusterKind_StillReturnsComponentsAndNode(t *testing.T) {
	b := newInstallInfoBundle(channelsdDeployment(1, 1), readyNode("n1"))
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
	c := authedClient(t, s)

	resp := doJSON(t, c, http.MethodGet, "http://"+s.Addr()+"/api/cluster/install-info", nil)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	var got installInfoResponse
	require.NoError(t, json.Unmarshal(body, &got))
	assert.False(t, got.ClusterDown)
	assert.Empty(t, got.ClusterKind)
	assert.Empty(t, got.OperatorEnv)
	assert.Contains(t, string(body), `"operatorEnv":[]`, "never null, even with no operator Deployment present")
	require.Len(t, got.Components, 1)
	assert.Equal(t, "channelsd", got.Components[0].Name)
	require.NotNil(t, got.Node)
}

func TestInstallInfo_ListError_500(t *testing.T) {
	cases := []struct {
		name     string
		resource string
	}{
		{name: "deployments list error", resource: "deployments"},
		{name: "nodes list error", resource: "nodes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newInstallInfoBundle(operatorDeployment(1, 1), readyNode("n1"))
			b.Typed.(*k8sfake.Clientset).PrependReactor("list", tc.resource,
				func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, assert.AnError })
			s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
			c := authedClient(t, s)

			resp := doJSON(t, c, http.MethodGet, "http://"+s.Addr()+"/api/cluster/install-info", nil)
			body := readBody(t, resp)
			assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "body: %s", body)
			assert.Contains(t, string(body), "list")
		})
	}
}
