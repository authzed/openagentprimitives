//go:build e2e

package e2e_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestInProcessFactory_PlanGateSlotTransformsReachesTheLoop guards the e2e
// harness's OWN Loop-construction call site
// (pkg/e2e/inprocess_runner_factory.go's buildLoop), not runner.SlotTransformsOf
// in isolation.
//
// runner.SlotTransformsOf has its own unit test
// (pkg/agent/runner/slot_value_chain_test.go), but that test cannot catch a
// regression where buildLoop stops calling it — e.g. the
// "PlanGateSlotTransforms: runner.SlotTransformsOf(class)" line is deleted, or
// a local hand-rolled copy is pasted back in — because it never drives
// buildLoop at all. This test does: it exercises the real factory
// construction path end to end and asserts the resulting Loop carries the
// class's published transform chain, so that exact regression fails here.
func TestInProcessFactory_PlanGateSlotTransformsReachesTheLoop(t *testing.T) {
	scripted := e2e.NewScriptedLLM(t)
	scripted.OnUserMessage("").Reply(e2e.EndTurn()).Repeating()

	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-transform-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "test", Name: "scripted",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are a test agent"},
		},
		Status: spiceboxv1alpha1.AgentClassStatus{
			ResolvedSlots: []spiceboxv1alpha1.ResolvedSlot{
				{ResourceType: "git_repo", Permission: "push", ValueTransforms: []string{"normalize_url", "spicedb_escape"}},
				{ResourceType: "tracker_issue", Permission: "read"}, // no chain: not value-keyed
			},
		},
	}
	// kubectl-style session (no InputChannel) so buildLoop needs no NATS/K8s —
	// same fixture shape as TestFactoryUsesCapabilityAssemble.
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-transform", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo-transform-agent",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hello"},
		},
	}

	f := &e2e.InProcessRunnerFactory{LLM: scripted}

	got, err := f.BuildLoopPlanGateSlotTransformsForTest(sess, class)
	require.NoError(t, err, "buildLoop")
	assert.Equal(t, map[string][]string{"git_repo": {"normalize_url", "spicedb_escape"}}, got,
		"the factory's own Loop construction must carry the class's published transform chain, "+
			"with the non-value-keyed slot absent rather than present with a nil/empty chain")
}

// TestInProcessFactory_PlanGateSlotStandingReachesTheLoop is
// TestInProcessFactory_PlanGateSlotTransformsReachesTheLoop's sibling for
// Loop.PlanGateSlotStanding, guarding this factory's OWN Loop-construction
// call site the same way: a unit test on runner.SlotStandingOf in isolation
// (pkg/agent/runner/slot_value_chain_test.go) cannot catch a regression where
// buildLoop stops calling it, because it never drives buildLoop at all.
func TestInProcessFactory_PlanGateSlotStandingReachesTheLoop(t *testing.T) {
	scripted := e2e.NewScriptedLLM(t)
	scripted.OnUserMessage("").Reply(e2e.EndTurn()).Repeating()

	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-standing-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "test", Name: "scripted",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are a test agent"},
		},
		Status: spiceboxv1alpha1.AgentClassStatus{
			ResolvedSlots: []spiceboxv1alpha1.ResolvedSlot{
				{ResourceType: "crm_company", Permission: "contact_access", Standing: spiceboxv1alpha1.StandingRequired},
				{ResourceType: "git_repo", Permission: "push"}, // no standing published: defaults session-only
			},
		},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-standing", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo-standing-agent",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hello"},
		},
	}

	f := &e2e.InProcessRunnerFactory{LLM: scripted}

	got, err := f.BuildLoopPlanGateSlotStandingForTest(sess, class)
	require.NoError(t, err, "buildLoop")
	assert.Equal(t, map[string]string{"crm_company": spiceboxv1alpha1.StandingRequired}, got,
		"the factory's own Loop construction must carry the class's published standing, "+
			"with the unpublished slot absent rather than present with an empty string")
}

// TestInProcessFactory_PlanGatePermissionTitlesReachesTheLoop is
// TestInProcessFactory_PlanGateSlotTransformsReachesTheLoop's sibling for
// Loop.PlanGatePermissionTitles, guarding this factory's OWN Loop-construction
// call site the same way: a unit test on runner.PermissionTitlesOf in
// isolation (pkg/agent/runner/slot_value_chain_test.go) cannot catch a
// regression where buildLoop stops calling it, because it never drives
// buildLoop at all. Without this seam, a declared permission title could
// silently stop reaching plangate.CardInput.PermissionTitles and every card
// would fall back to the detokenized handle with no test failing.
func TestInProcessFactory_PlanGatePermissionTitlesReachesTheLoop(t *testing.T) {
	scripted := e2e.NewScriptedLLM(t)
	scripted.OnUserMessage("").Reply(e2e.EndTurn()).Repeating()

	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-title-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "test", Name: "scripted",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are a test agent"},
		},
		Status: spiceboxv1alpha1.AgentClassStatus{
			ResolvedPermissionTitles: []spiceboxv1alpha1.ResolvedPermissionTitle{
				{ResourceType: "git_repo", Permission: "push", Title: "Push commits to the repository"},
			},
		},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-title", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo-title-agent",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hello"},
		},
	}

	f := &e2e.InProcessRunnerFactory{LLM: scripted}

	got, err := f.BuildLoopPlanGatePermissionTitlesForTest(sess, class)
	require.NoError(t, err, "buildLoop")
	assert.Equal(t, map[string]string{"git_repo/push": "Push commits to the repository"}, got,
		"the factory's own Loop construction must carry the class's published permission titles, "+
			`keyed "<resourceType>/<permission>"`)
}

// TestInProcessFactory_PlanGateResourceDisplaysReachesTheLoop is
// TestInProcessFactory_PlanGatePermissionTitlesReachesTheLoop's sibling for
// Loop.PlanGateResourceDisplays, guarding this factory's OWN Loop-construction
// call site the same way: a unit test on runner.ResourceDisplaysOf in
// isolation (pkg/agent/runner/slot_value_chain_test.go) cannot catch a
// regression where buildLoop stops calling it, because it never drives
// buildLoop at all. Without this seam, a declared resource display could
// silently stop reaching plangate.CardInput.ResourceDisplays and every
// resource line would fall back to the wire type name with no test failing.
func TestInProcessFactory_PlanGateResourceDisplaysReachesTheLoop(t *testing.T) {
	scripted := e2e.NewScriptedLLM(t)
	scripted.OnUserMessage("").Reply(e2e.EndTurn()).Repeating()

	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-display-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "test", Name: "scripted",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are a test agent"},
		},
		Status: spiceboxv1alpha1.AgentClassStatus{
			ResolvedResourceDisplays: []spiceboxv1alpha1.ResolvedResourceDisplay{
				{ResourceType: "git_repo", Name: "Git repository", Icon: "repository", Label: "url_path"},
			},
		},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-display", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo-display-agent",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hello"},
		},
	}

	f := &e2e.InProcessRunnerFactory{LLM: scripted}

	got, err := f.BuildLoopPlanGateResourceDisplaysForTest(sess, class)
	require.NoError(t, err, "buildLoop")
	assert.Equal(t, map[string]plangate.ResourceDisplay{
		"git_repo": {Name: "Git repository", Icon: "repository", Label: "url_path"},
	}, got, "the factory's own Loop construction must carry the class's published resource displays, keyed by resourceType")
}
