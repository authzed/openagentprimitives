//go:build e2e

package e2e_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestFactoryUsesCapabilityAssemble is a guard: the in-process factory must
// build its tool list via capability.Assemble, not a hand-maintained list.
//
// The proof is behavioral. query_memory can only reach a session's tool set
// through the OPT-IN "memory" capability's Offer (the pre-Assemble factory
// never built it). So:
//   - a class that GRANTS "memory" must yield query_memory, and
//   - an otherwise-identical class that does NOT grant it must not,
//
// which is only possible if the factory routes assembly through the capability
// registry. A stray hand-rolled list would either always emit query_memory or
// never — never keyed on the grant.
func TestFactoryUsesCapabilityAssemble(t *testing.T) {
	scripted := e2e.NewScriptedLLM(t)
	scripted.OnUserMessage("").Reply(e2e.EndTurn()).Repeating()

	baseClass := func() *spiceboxv1alpha1.AgentClass {
		return &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
			Spec: spiceboxv1alpha1.AgentClassSpec{
				Model: &spiceboxv1alpha1.ModelConfig{
					Provider: "test", Name: "scripted",
					APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
				},
				SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are a test agent"},
			},
		}
	}
	// kubectl-style session (no InputChannel) so buildLoop needs no NATS/K8s.
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "test-agent",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hello"},
		},
	}

	f := &e2e.InProcessRunnerFactory{LLM: scripted}

	// Granted: the memory capability activates (MemoryAvailable is always true
	// in the in-process factory), so query_memory appears.
	granting := baseClass()
	granting.Spec.Capabilities = map[string]apiextensionsv1.JSON{
		"memory": {Raw: []byte(`{}`)},
	}
	grantedNames, err := f.BuildLoopToolNamesForTest(sess, granting)
	require.NoError(t, err, "buildLoop with memory grant")
	assert.Contains(t, grantedNames, "query_memory",
		"granting the memory capability must yield query_memory (proves routing through Assemble)")

	// Not granted: memory is opt-in, so the only way query_memory could appear
	// is a hand-rolled list bypassing the registry.
	ungranted := baseClass()
	ungrantedNames, err := f.BuildLoopToolNamesForTest(sess, ungranted)
	require.NoError(t, err, "buildLoop without memory grant")
	assert.NotContains(t, ungrantedNames, "query_memory",
		"a class not granting memory must not get query_memory")

	// Sanity: the infrastructural core capability's terminal tools are always
	// present regardless of grants — confirms Assemble ran (not an empty list).
	assert.Contains(t, ungrantedNames, "agent_work_complete",
		"core capability's agent_work_complete must always be present")
	assert.Contains(t, ungrantedNames, "new_operation",
		"core capability's new_operation must always be present")
}
