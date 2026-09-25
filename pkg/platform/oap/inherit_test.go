package oap

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	// Registers the static/oauth/federated/githubApp credkind.Kinds so
	// collectIdentitySecrets's registry dispatch (credkindregistry.Get) resolves
	// the fixtures below instead of failing closed on every one as an
	// unregistered type — a test binary is its own process, and another
	// package's blank import of credkind/imports does not leak in.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

// agentClassU builds a minimal AgentClass unstructured for derivation tests.
func agentClassU(name string, mutate func(spec map[string]any)) *unstructured.Unstructured {
	spec := map[string]any{}
	if mutate != nil {
		mutate(spec)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentClass",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}
}

func agentIdentityU(name string, creds []any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentIdentity",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"credentials": creds},
	}}
}

func TestDeriveInherited_IdentityAndSkills(t *testing.T) {
	ac := agentClassU("demo-agent", func(spec map[string]any) {
		spec["displayName"] = "Demo Agent"
		spec["description"] = "does demo things"
		spec["skills"] = []any{
			map[string]any{"name": "planner", "ref": "github.com/fakeorg/fakerepo//skills/planner@v1"},
		}
	})

	agent, req, err := DeriveInherited([]*unstructured.Unstructured{ac}, logr.Discard())
	require.NoError(t, err)

	assert.Equal(t, "demo-agent", agent.Name)
	assert.Equal(t, "Demo Agent", agent.DisplayName)
	assert.Equal(t, "does demo things", agent.Description)
	assert.Empty(t, agent.Version, "Version is not an AgentClass fact; the caller supplies it")
	require.Len(t, req.Skills, 1)
	assert.Equal(t, "github.com/fakeorg/fakerepo//skills/planner@v1", req.Skills[0].Canonical)
}

func TestDeriveInherited_SecretsFromStaticCredentialWithPurpose(t *testing.T) {
	ac := agentClassU("demo-agent", func(spec map[string]any) {
		spec["agentIdentity"] = "demo-agent-id"
		spec["credentialExplanations"] = []any{
			map[string]any{"credential": "api-cred", "reason": "why we need the api key"},
		}
	})
	ai := agentIdentityU("demo-agent-id", []any{
		map[string]any{
			"name":   "api-cred",
			"type":   "static",
			"static": map[string]any{"secretRef": map[string]any{"name": "demo-agent-secret", "key": "api-key"}},
		},
	})

	_, req, err := DeriveInherited([]*unstructured.Unstructured{ac, ai}, logr.Discard())
	require.NoError(t, err)

	require.Len(t, req.Secrets, 1)
	assert.Equal(t, "demo-agent-secret", req.Secrets[0].Name)
	assert.Equal(t, []string{"api-key"}, req.Secrets[0].Keys)
	assert.Equal(t, "why we need the api key", req.Secrets[0].Purpose)
}

func TestDeriveInherited_ExactlyOneAgentClass(t *testing.T) {
	one := agentClassU("a", nil)
	two := agentClassU("b", nil)

	_, _, errNone := DeriveInherited(nil, logr.Discard())
	assert.ErrorContains(t, errNone, "exactly one AgentClass")

	_, _, errTwo := DeriveInherited([]*unstructured.Unstructured{one, two}, logr.Discard())
	assert.ErrorContains(t, errTwo, "exactly one AgentClass")
}
