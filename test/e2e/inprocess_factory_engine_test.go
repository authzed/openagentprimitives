//go:build e2e

package e2e_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/test/e2e"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// TestInProcessFactory_EngineReachesTheLoop guards the e2e harness's OWN
// Loop-construction call site: the `if f.SpiceDB != nil` block in
// inprocess_runner_factory.go's buildLoop must wire loop.Engine, exactly as
// production does at internal/cmd/runner/main.go.
//
// This is the prerequisite for every observed-slot bundle. promoteObservedSlots
// (pkg/agent/runner/loop_autofill.go) returns at its first guard when
// loop.Engine == nil, so a nil Engine silently no-ops promotion: a
// fillFrom:[observed] slot never binds, and a fork bundle asserting on that
// binding would pass while testing nothing. A unit test on the engine in
// isolation cannot catch a harness that never wires it — this seam drives the
// factory's construction path end to end and fails if the Engine goes missing.
//
// The observed-source slot in the fixture is documentation of WHY a nil Engine
// is a bug; the assertion itself does not depend on it, because the Engine is
// wired unconditionally inside the SpiceDB block.
func TestInProcessFactory_EngineReachesTheLoop(t *testing.T) {
	scripted := e2e.NewScriptedLLM(t)
	scripted.OnUserMessage("").Reply(e2e.EndTurn()).Repeating()

	// A real *spicedb.Client is required: the wiring is gated on the concrete
	// f.SpiceDB, so a fake cannot enter the block. buildLoop performs no SpiceDB
	// RPC (it constructs the Engine, it does not Check through it), so the
	// container needs no bootstrapped schema for this construction-only path.
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	spdbCli, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err, "spicedb.NewClient")

	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-observed-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "test", Name: "scripted",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are a test agent"},
			Authz: &spiceboxv1alpha1.AuthzBlock{
				Slots: []spiceboxv1alpha1.AuthzSlot{
					{
						ResourceType: "crm_company",
						Description:  "A CRM company record",
						Permission:   "contact_access",
						FillFrom:     []string{"observed"},
					},
				},
			},
		},
	}
	// kubectl-style session (no InputChannel) so buildLoop needs no NATS/K8s —
	// same fixture shape as the plangate slot-transform seam tests.
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-observed", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo-observed-agent",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hello"},
		},
	}

	f := &e2e.InProcessRunnerFactory{LLM: scripted, SpiceDB: spdbCli}

	wired, err := f.BuildLoopEngineWiredForTest(sess, class)
	require.NoError(t, err, "buildLoop")
	assert.True(t, wired,
		"the factory's own Loop construction must wire loop.Engine when SpiceDB is present, "+
			"or promoteObservedSlots no-ops at its Engine==nil guard and no fillFrom:[observed] slot ever binds")
}
