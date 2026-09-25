package agentsession_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/harness/apnative"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
)

// The runner composes the durable artifact link a trigger's status surface
// carries — a GitHub check run's details link — and needs webd's externally
// reachable address to do it. A pod cannot read the ConfigMap that holds it:
// it lives in agentprimitives-system and runner pods live in the session's
// namespace. The operator, which already watches that ConfigMap, carries the
// value across the boundary the same way it carries OPERATOR_MEMORY_URL.
//
// The env NAME is asserted through externalurl.EnvWebdBaseURL rather than
// spelled here, because that constant is the only thing keeping this stamp and
// the runner's flag binding in agreement — a literal on either side would let
// them drift into a runner that silently never has the URL.
func webdEnv(t *testing.T, webdBaseURL string) (corev1.EnvVar, bool) {
	t.Helper()
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session: sessWithAPIKey("webd1", "llm-creds", "api-key"),
		Class: &spiceboxv1alpha1.AgentClass{
			Spec: spiceboxv1alpha1.AgentClassSpec{
				Model: &spiceboxv1alpha1.ModelConfig{
					APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
				},
			},
		},
		Image:       "agentprimitives-runner:dev",
		ServiceAcct: "webd1-runner-sa",
		MemoryToken: "webd1-memory-token",
		OperatorURL: "http://op:8082",
		WebdBaseURL: webdBaseURL,
		Harness:     apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == externalurl.EnvWebdBaseURL {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

func TestPodSpecCarriesWebdBaseURLToTheRunner(t *testing.T) {
	got, ok := webdEnv(t, "https://ap.example")
	require.True(t, ok, "%s must be injected when the operator knows webd's address", externalurl.EnvWebdBaseURL)
	assert.Nil(t, got.ValueFrom, "a plain value: the pod cannot reach a ConfigMap in another namespace")
	assert.Equal(t, "https://ap.example", got.Value)
}

// TestPodSpecOmitsWebdBaseURLWhenUnknown: `oap install` seeds webd's
// external-URL ConfigMap empty while it is still arranging external access, so
// "the operator does not know yet" is a live state, not a misconfiguration.
// Stamping an empty value would hand the runner a base URL it would then have
// to special-case; omitting the variable lets its own default do that once.
func TestPodSpecOmitsWebdBaseURLWhenUnknown(t *testing.T) {
	_, ok := webdEnv(t, "")
	assert.False(t, ok, "%s must be absent rather than empty when webd has no external address yet", externalurl.EnvWebdBaseURL)
}
