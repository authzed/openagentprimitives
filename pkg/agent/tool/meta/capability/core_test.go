package capability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// toolNames is a shared test helper for extracting tool names from a slice.
func toolNames(tools []tool.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name())
	}
	return names
}

func TestCoreAlwaysOffersTerminalTools(t *testing.T) {
	c, ok := Lookup("core")
	assert.True(t, ok)
	assert.True(t, c.Infrastructural())
	tools, skip := c.Offer(OfferContext{Ctx: context.Background()})
	assert.Nil(t, skip)
	names := toolNames(tools)
	assert.Contains(t, names, "agent_work_complete")
	assert.Contains(t, names, "new_operation")
}

func TestCoreChannelAwareSwap(t *testing.T) {
	c, _ := Lookup("core")
	tools, _ := c.Offer(OfferContext{Ctx: context.Background(), Env: RunnerEnv{ChannelAttached: true, NATSPublish: func(context.Context, string, []byte) error { return nil }}})
	assert.Contains(t, toolNames(tools), "agent_work_complete", "channel-aware variant keeps the same tool name")
}

func TestPlanningDefaultOn(t *testing.T) {
	c, ok := Lookup("planning")
	assert.True(t, ok)
	assert.True(t, c.DefaultOn())
	tools, skip := c.Offer(OfferContext{Ctx: context.Background()})
	assert.Nil(t, skip)
	assert.Contains(t, toolNames(tools), "update_plan")
}

// TestCoreDelegatedChildGetsReturnResultInstead pins the swap: a session with
// spec.parent gets return_result and does NOT keep agent_work_complete.
//
// Replaced rather than supplemented on purpose — two ways to finish, one of
// them written for a session with a human reader, is how a child ends up
// calling the wrong one and returning a status report to its caller.
//
// Keyed on spec.parent ALONE, not on a conversational binding: a single_turn
// child returns through the same field and predates conversational modes.
func TestCoreDelegatedChildGetsReturnResultInstead(t *testing.T) {
	c, _ := Lookup("core")

	child := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "child-1", Namespace: "demo-ns"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo-helper",
			Parent: &spiceboxv1alpha1.NamespacedRef{Namespace: "demo-ns", Name: "root-1"},
		},
	}
	root := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "root-1", Namespace: "demo-ns"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-lead"},
	}

	childTools, _ := c.Offer(OfferContext{Ctx: context.Background(), Session: child})
	rootTools, _ := c.Offer(OfferContext{Ctx: context.Background(), Session: root})

	assert.Contains(t, toolNames(childTools), "return_result",
		"a delegated child's terminal tool must be the one that says its payload is the answer")
	assert.NotContains(t, toolNames(childTools), "agent_work_complete",
		"replaced, not supplemented — leaving both lets the child pick the one written for a human session")

	assert.Contains(t, toolNames(rootTools), "agent_work_complete",
		"a root session keeps the tool whose summary really is audit-only")
	assert.NotContains(t, toolNames(rootTools), "return_result",
		"a session with a human reader has no caller to return to")
}
