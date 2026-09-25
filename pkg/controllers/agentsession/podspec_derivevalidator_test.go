package agentsession_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/harness/apnative"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
)

// runnerEnvMap builds a runner pod and returns its first container's env as a
// name->value map, for asserting on individual variables.
func runnerEnvMap(t *testing.T) map[string]string {
	t.Helper()
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session: sessWithAPIKey("dv1", "llm-creds", "api-key"),
		Class: &spiceboxv1alpha1.AgentClass{
			Spec: spiceboxv1alpha1.AgentClassSpec{
				Model: &spiceboxv1alpha1.ModelConfig{
					APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
				},
			},
		},
		Image:       "agentprimitives-runner:dev",
		ServiceAcct: "dv1-runner-sa",
		MemoryToken: "dv1-memory-token",
		OperatorURL: "http://op:8082",
		Harness:     apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")
	m := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		m[e.Name] = e.Value
	}
	return m
}

// TestPodSpecForwardsDeriveValidatorEnvToTheRunner: the dedicated
// derivation-validator model (derive_tag) is cluster-wide config the operator
// holds in its OWN env; the runner reads it from ITS pod env at startup, so the
// operator must copy the three PT_DERIVE_VALIDATOR_* vars across the
// namespace boundary — the same shape as DEV_RELWRITES_USER_EMAIL. Without this
// the runner reads them empty and never offers derive_tag, so the whole
// derivation path is dead in a deployed session even when configured.
func TestPodSpecForwardsDeriveValidatorEnvToTheRunner(t *testing.T) {
	t.Setenv("PT_DERIVE_VALIDATOR_PROVIDER", "anthropic")
	t.Setenv("PT_DERIVE_VALIDATOR_MODEL", "claude-haiku-4-5-20251001")
	t.Setenv("PT_DERIVE_VALIDATOR_API_KEY", "sk-validator-xyz")

	env := runnerEnvMap(t)
	assert.Equal(t, "anthropic", env["PT_DERIVE_VALIDATOR_PROVIDER"])
	assert.Equal(t, "claude-haiku-4-5-20251001", env["PT_DERIVE_VALIDATOR_MODEL"])
	assert.Equal(t, "sk-validator-xyz", env["PT_DERIVE_VALIDATOR_API_KEY"],
		"the runner reads the validator key from its own env; a file mount is the preview-hardening follow-up")
}

// TestPodSpecOmitsDeriveValidatorEnvWhenUnset: unset must mean the variable is
// ABSENT, not present-and-empty — an empty PROVIDER would make providers.New
// fail loudly at runner startup instead of cleanly leaving derive_tag unoffered.
func TestPodSpecOmitsDeriveValidatorEnvWhenUnset(t *testing.T) {
	t.Setenv("PT_DERIVE_VALIDATOR_PROVIDER", "")
	t.Setenv("PT_DERIVE_VALIDATOR_MODEL", "")
	t.Setenv("PT_DERIVE_VALIDATOR_API_KEY", "")

	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session:     sessWithAPIKey("dv2", "llm-creds", "api-key"),
		Class:       &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{Model: &spiceboxv1alpha1.ModelConfig{APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"}}}},
		Image:       "agentprimitives-runner:dev",
		ServiceAcct: "dv2-runner-sa",
		MemoryToken: "dv2-memory-token",
		OperatorURL: "http://op:8082",
		Harness:     apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")
	for _, e := range pod.Spec.Containers[0].Env {
		assert.NotContains(t, e.Name, "PT_DERIVE_VALIDATOR_",
			"unset validator vars must be omitted, not stamped empty (got %s=%q)", e.Name, e.Value)
	}
}
